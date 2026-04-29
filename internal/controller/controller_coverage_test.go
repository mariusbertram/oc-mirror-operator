package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog/builder"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func mustGzipJSON(v interface{}) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	Expect(json.NewEncoder(gz).Encode(v)).To(Succeed())
	Expect(gz.Close()).To(Succeed())
	return buf.Bytes()
}

func cleanupMT(ctx context.Context, name string) {
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, mt); err == nil {
		if controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
			controllerutil.RemoveFinalizer(mt, mirrorTargetFinalizer)
			_ = k8sClient.Update(ctx, mt)
		}
		_ = k8sClient.Delete(ctx, mt)
	}
}

var _ = Describe("Coverage tests", func() {
	const ns = "default"

	// ───────────────────── Pure function tests ─────────────────────

	Describe("cleanupJobName", func() {
		It("returns a deterministic DNS-safe name", func() {
			name := cleanupJobName("my-target", "my-imageset")
			Expect(name).To(HavePrefix("cleanup-my-target-my-imageset-"))
			Expect(len(name)).To(BeNumerically("<=", 63))
		})

		It("is deterministic across calls", func() {
			Expect(cleanupJobName("t", "is")).To(Equal(cleanupJobName("t", "is")))
		})

		It("different inputs produce different names", func() {
			Expect(cleanupJobName("t", "a")).NotTo(Equal(cleanupJobName("t", "b")))
		})

		It("truncates long names to 63 chars", func() {
			long := "this-is-a-very-long-name-that-exceeds-dns-limits-for-kubernetes-resources"
			name := cleanupJobName(long, long)
			Expect(len(name)).To(BeNumerically("<=", 63))
		})
	})

	Describe("caBundleEnvVars", func() {
		It("returns nil when ref is nil", func() {
			Expect(caBundleEnvVars(nil)).To(BeNil())
		})

		It("uses default key when Key is empty", func() {
			env := caBundleEnvVars(&mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"})
			Expect(env).To(HaveLen(1))
			Expect(env[0].Value).To(Equal("/run/secrets/ca/ca-bundle.crt"))
		})

		It("uses custom key when set", func() {
			env := caBundleEnvVars(&mirrorv1alpha1.CABundleRef{ConfigMapName: "ca", Key: "custom.pem"})
			Expect(env).To(HaveLen(1))
			Expect(env[0].Value).To(Equal("/run/secrets/ca/custom.pem"))
		})
	})

	Describe("managerContainerVolumeMounts", func() {
		It("returns empty when no auth and no CA", func() {
			mt := &mirrorv1alpha1.MirrorTarget{}
			Expect(managerContainerVolumeMounts(mt)).To(BeEmpty())
		})

		It("includes dockerconfig mount when AuthSecret is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{AuthSecret: "s"}}
			mounts := managerContainerVolumeMounts(mt)
			Expect(mounts).To(HaveLen(1))
			Expect(mounts[0].Name).To(Equal("dockerconfig"))
		})

		It("includes ca-bundle mount when CABundle is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"},
			}}
			mounts := managerContainerVolumeMounts(mt)
			Expect(mounts).To(HaveLen(1))
			Expect(mounts[0].Name).To(Equal("ca-bundle"))
		})

		It("includes both when both are set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				AuthSecret: "s",
				CABundle:   &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"},
			}}
			Expect(managerContainerVolumeMounts(mt)).To(HaveLen(2))
		})
	})

	Describe("managerPodVolumes", func() {
		It("returns empty when no auth and no CA", func() {
			Expect(managerPodVolumes(&mirrorv1alpha1.MirrorTarget{})).To(BeEmpty())
		})

		It("includes dockerconfig volume when AuthSecret is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{AuthSecret: "my-secret"}}
			vols := managerPodVolumes(mt)
			Expect(vols).To(HaveLen(1))
			Expect(vols[0].Name).To(Equal("dockerconfig"))
			Expect(vols[0].Secret.SecretName).To(Equal("my-secret"))
		})

		It("includes ca-bundle volume with default key", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "my-ca"},
			}}
			vols := managerPodVolumes(mt)
			Expect(vols).To(HaveLen(1))
			Expect(vols[0].Name).To(Equal("ca-bundle"))
			Expect(vols[0].ConfigMap.Items[0].Key).To(Equal("ca-bundle.crt"))
		})

		It("uses custom CA key", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca", Key: "custom.pem"},
			}}
			vols := managerPodVolumes(mt)
			Expect(vols[0].ConfigMap.Items[0].Key).To(Equal("custom.pem"))
		})

		It("includes both volumes when both are set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				AuthSecret: "s",
				CABundle:   &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"},
			}}
			Expect(managerPodVolumes(mt)).To(HaveLen(2))
		})
	})

	Describe("managerContainerEnv", func() {
		It("includes DOCKER_CONFIG when AuthSecret is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{AuthSecret: "s"}}
			env := managerContainerEnv(mt)
			var found bool
			for _, e := range env {
				if e.Name == "DOCKER_CONFIG" {
					found = true
					Expect(e.Value).To(Equal("/docker-config"))
				}
			}
			Expect(found).To(BeTrue())
		})

		It("omits DOCKER_CONFIG when no AuthSecret", func() {
			env := managerContainerEnv(&mirrorv1alpha1.MirrorTarget{})
			for _, e := range env {
				Expect(e.Name).NotTo(Equal("DOCKER_CONFIG"))
			}
		})

		It("includes SSL_CERT_FILE when CABundle is set", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{
				CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"},
			}}
			env := managerContainerEnv(mt)
			var found bool
			for _, e := range env {
				if e.Name == "SSL_CERT_FILE" {
					found = true
				}
			}
			Expect(found).To(BeTrue())
		})
	})

	// ───────────────────── operatorImagesMirrored (fake client) ─────────────────────

	Describe("operatorImagesMirrored", func() {
		var (
			fakeScheme *runtime.Scheme
			bgCtx      context.Context
		)

		BeforeEach(func() {
			fakeScheme = runtime.NewScheme()
			Expect(mirrorv1alpha1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(corev1.AddToScheme(fakeScheme)).To(Succeed())
			bgCtx = context.Background()
		})

		It("returns (false, false) when no ConfigMap exists", func() {
			c := fake.NewClientBuilder().WithScheme(fakeScheme).Build()
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "no-cm", Namespace: ns}}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeFalse())
			Expect(know).To(BeFalse())
		})

		It("returns (true, true) when all operator images are Mirrored", func() {
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator},
				"d2": {Source: "s2", State: "Mirrored", Origin: imagestate.OriginOperator},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "all-mirrored-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "all-mirrored", Namespace: ns}}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeTrue())
			Expect(know).To(BeTrue())
		})

		It("returns (false, true) when some operator images are pending", func() {
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator},
				"d2": {Source: "s2", State: "Pending", Origin: imagestate.OriginOperator},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "partial-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "partial", Namespace: ns}}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeFalse())
			Expect(know).To(BeTrue())
		})

		It("returns (false, true) when no operator-origin entries exist", func() {
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginRelease},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "no-ops-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "no-ops", Namespace: ns}}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeFalse())
			Expect(know).To(BeTrue())
		})

		It("treats PermanentlyFailed operator images as done", func() {
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Failed", Origin: imagestate.OriginOperator, PermanentlyFailed: true},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "permfail-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "permfail", Namespace: ns}}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeTrue())
			Expect(know).To(BeTrue())
		})
	})

	// ───────────────────── findOwningMirrorTarget ─────────────────────

	Describe("findOwningMirrorTarget edge cases", func() {
		It("returns error when multiple MirrorTargets reference the same ImageSet", func() {
			localCtx := context.Background()
			isName := "is-multi-owner"

			mt1 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt1-multi-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "r1", ImageSets: []string{isName}},
			}
			mt2 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt2-multi-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "r2", ImageSets: []string{isName}},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}

			Expect(k8sClient.Create(localCtx, mt1)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt1) })
			Expect(k8sClient.Create(localCtx, mt2)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt2) })
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.findOwningMirrorTarget(localCtx, is)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("multiple MirrorTargets"))
		})
	})

	// ───────────────────── ImageSet Reconcile edge cases ─────────────────────

	Describe("ImageSet Reconcile edge cases", func() {
		It("returns no error for non-existent ImageSet", func() {
			r := newImageSetReconciler()
			result, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent-is-coverage", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	// ───────────────────── reconcileCatalogBuildJobs ─────────────────────

	Describe("reconcileCatalogBuildJobs", func() {
		It("sets WaitingForOperatorMirror when gate is closed (no imagestate)", func() {
			localCtx := context.Background()
			isName := "is-catgate-closed"
			mtName := "mt-catgate-closed"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "quay.io/redhat/catalog:v4.21"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.reconcileCatalogBuildJobs(localCtx, is, mt, false)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var found bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == "WaitingForOperatorMirror" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected CatalogReady=WaitingForOperatorMirror condition")
		})

		It("creates build jobs when gate is open via recollect annotation", func() {
			localCtx := context.Background()
			isName := "is-catgate-recollect"
			mtName := "mt-catgate-recollect"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:        isName,
					Namespace:   ns,
					Annotations: map[string]string{mirrorv1alpha1.RecollectAnnotation: ""},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{
								Catalog: "quay.io/redhat/catalog:v4.21",
								IncludeConfig: mirrorv1alpha1.IncludeConfig{
									Packages: []mirrorv1alpha1.IncludePackage{{Name: "web-terminal"}},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			jobName := builder.JobName(isName, "quay.io/redhat/catalog:v4.21")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundRunning bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == "CatalogBuildRunning" {
					foundRunning = true
				}
			}
			Expect(foundRunning).To(BeTrue(), "expected CatalogReady=CatalogBuildRunning")
		})

		It("sets CatalogReady=True when all jobs succeeded", func() {
			localCtx := context.Background()
			isName := "is-catbuild-succeed"
			mtName := "mt-catbuild-succeed"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:        isName,
					Namespace:   ns,
					Annotations: map[string]string{mirrorv1alpha1.RecollectAnnotation: ""},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "quay.io/test/catalog:v1"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// Pre-create the job with Succeeded status
			jobName := builder.JobName(isName, "quay.io/test/catalog:v1")
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      jobName,
					Namespace: ns,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "oc-mirror-operator"},
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "test", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(localCtx, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			fresh := &mirrorv1alpha1.ImageSet{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, fresh)).To(Succeed())
			var foundReady bool
			for _, c := range fresh.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Status == metav1.ConditionTrue {
					foundReady = true
				}
			}
			Expect(foundReady).To(BeTrue(), "expected CatalogReady=True")
		})

		It("sets CatalogBuildFailed when a job has failed", func() {
			localCtx := context.Background()
			isName := "is-catbuild-fail"
			mtName := "mt-catbuild-fail"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:        isName,
					Namespace:   ns,
					Annotations: map[string]string{mirrorv1alpha1.RecollectAnnotation: ""},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "quay.io/test/failcat:v1"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// Pre-create the job with Failed status
			jobName := builder.JobName(isName, "quay.io/test/failcat:v1")
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "test", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(localCtx, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundFailed bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == "CatalogBuildFailed" {
					foundFailed = true
				}
			}
			Expect(foundFailed).To(BeTrue(), "expected CatalogReady=CatalogBuildFailed")
		})

		It("deletes old job and rebuilds when build signature changes", func() {
			localCtx := context.Background()
			isName := "is-catbuild-rebuild"
			mtName := "mt-catbuild-rebuild"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation:      "",
						"mirror.openshift.io/catalog-build-sig": "stale-sig",
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "quay.io/test/rebuildcat:v1"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// Pre-create an old job
			jobName := builder.JobName(isName, "quay.io/test/rebuildcat:v1")
			oldJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "test", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, oldJob)).To(Succeed())
			oldJob.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(localCtx, oldJob)).To(Succeed())

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			// Re-read to get latest ResourceVersion after creation
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			// The old job should be deleted and a new one created (or job recreated).
			// Verify the annotation was updated with the new build sig.
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(is.Annotations["mirror.openshift.io/catalog-build-sig"]).NotTo(Equal("stale-sig"))

			DeferCleanup(func() {
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})

		It("forces rebuild when poll expires", func() {
			localCtx := context.Background()
			isName := "is-catbuild-poll"
			mtName := "mt-catbuild-poll"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			// Compute expected signature so lastSig == buildSig (no sig-based rebuild).
			ops := []mirrorv1alpha1.Operator{{Catalog: "quay.io/test/pollcat:v1"}}
			buildSig := bm.BuildSignature(ops)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation:      "",
						"mirror.openshift.io/catalog-build-sig": buildSig,
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{Operators: ops}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			// pollExpired=true triggers rebuild even though sig matches
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, true)).To(Succeed())

			jobName := builder.JobName(isName, "quay.io/test/pollcat:v1")
			DeferCleanup(func() {
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})

		It("skips rebuild when CatalogReady=True and sig unchanged", func() {
			localCtx := context.Background()
			isName := "is-catbuild-skip"
			mtName := "mt-catbuild-skip"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			ops := []mirrorv1alpha1.Operator{{Catalog: "quay.io/test/skipcat:v1"}}
			buildSig := bm.BuildSignature(ops)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						"mirror.openshift.io/catalog-build-sig": buildSig,
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{Operators: ops}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// Set CatalogReady=True on status
			is.Status.Conditions = []metav1.Condition{
				{Type: conditionCatalogReady, Status: metav1.ConditionTrue, Reason: "CatalogBuildSucceeded",
					Message: "ok", LastTransitionTime: metav1.Now()},
			}
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			// No job should be created since catalog is already ready and sig unchanged
			jobName := builder.JobName(isName, "quay.io/test/skipcat:v1")
			j := &batchv1.Job{}
			err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j)
			Expect(err).To(HaveOccurred(), "job should not exist when catalog is already ready")
		})
	})

	// ───────────────────── reconcileCleanup ─────────────────────

	Describe("reconcileCleanup", func() {
		It("creates cleanup job when ImageSet is removed and cleanup-policy is Delete", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-create"
			removedIS := "is-cleanup-removed"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{"is-keep"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			// Set KnownImageSets in memory (simulates previous reconcile state)
			mt.Status.KnownImageSets = []string{"is-keep", removedIS}

			// Create consolidated per-MirrorTarget state with the removed IS's exclusive images
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Refs: []imagestate.ImageRef{{ImageSet: removedIS, Origin: imagestate.OriginAdditional}}},
			}
			consolidatedCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, consolidatedCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, consolidatedCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			// Verify snapshot ConfigMap was created with the exclusive images
			snapshotName := cleanupSnapshotCMName(mtName, removedIS)
			snapshotCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: snapshotName, Namespace: ns}, snapshotCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, snapshotCM) })

			// Verify cleanup job was created
			jobName := cleanupJobName(mtName, removedIS)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(mt.Status.PendingCleanup).To(ContainElement(removedIS))
		})

		It("skips cleanup when cleanup-policy is not Delete", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-nopolicy"
			removedIS := "is-cleanup-nopol"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{"is-keep-np"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			mt.Status.KnownImageSets = []string{"is-keep-np", removedIS}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			// No job should be created
			jobName := cleanupJobName(mtName, removedIS)
			j := &batchv1.Job{}
			err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j)
			Expect(err).To(HaveOccurred())
		})

		It("removes succeeded cleanup from PendingCleanup", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-done"
			cleanedIS := "is-cleanup-done"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{"is-keep-done"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			// Pre-create a succeeded cleanup job
			jobName := cleanupJobName(mtName, cleanedIS)
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "cleanup", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(localCtx, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			mt.Status.KnownImageSets = []string{"is-keep-done"}
			mt.Status.PendingCleanup = []string{cleanedIS}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			Expect(mt.Status.PendingCleanup).NotTo(ContainElement(cleanedIS))
		})

		It("re-queues failed cleanup job", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-retry"
			failedIS := "is-cleanup-retry"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{"is-keep-retry"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			// Pre-create a failed cleanup job
			jobName := cleanupJobName(mtName, failedIS)
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "cleanup", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(localCtx, job)).To(Succeed())

			mt.Status.KnownImageSets = []string{"is-keep-retry"}
			mt.Status.PendingCleanup = []string{failedIS}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			// Failed IS stays in PendingCleanup for retry
			Expect(mt.Status.PendingCleanup).To(ContainElement(failedIS))
		})
	})

	// ───────────────────── createCleanupJob ─────────────────────

	Describe("createCleanupJob", func() {
		It("creates a job with correct labels and args", func() {
			localCtx := context.Background()
			mtName := "mt-create-cleanup"
			isName := "is-create-cleanup"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			snapshotCM := cleanupSnapshotCMName(mtName, isName)
			Expect(r.createCleanupJob(localCtx, mt, isName, snapshotCM)).To(Succeed())

			jobName := cleanupJobName(mtName, isName)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(job.Labels).To(HaveKeyWithValue("mirror.openshift.io/cleanup", isName))
			Expect(job.Labels).To(HaveKeyWithValue("mirrortarget", mtName))
			Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElements("cleanup", "--configmap", snapshotCM))
		})

		It("is a no-op when the job already exists", func() {
			localCtx := context.Background()
			mtName := "mt-create-cleanup-noop"
			isName := "is-create-cleanup-noop"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			snapshotCM := cleanupSnapshotCMName(mtName, isName)
			Expect(r.createCleanupJob(localCtx, mt, isName, snapshotCM)).To(Succeed())
			// Call again — should be no-op
			Expect(r.createCleanupJob(localCtx, mt, isName, snapshotCM)).To(Succeed())

			DeferCleanup(func() {
				jobName := cleanupJobName(mtName, isName)
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})

		It("includes auth volume when AuthSecret is set", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-auth"
			isName := "is-cleanup-auth"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:   "reg.example.com",
					AuthSecret: "my-auth-secret",
					Insecure:   true,
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			snapshotCM := cleanupSnapshotCMName(mtName, isName)
			Expect(r.createCleanupJob(localCtx, mt, isName, snapshotCM)).To(Succeed())

			jobName := cleanupJobName(mtName, isName)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			// Verify auth volume and --insecure flag
			Expect(job.Spec.Template.Spec.Volumes).NotTo(BeEmpty())
			Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--insecure"))
			var hasDOCKER bool
			for _, e := range job.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "DOCKER_CONFIG" {
					hasDOCKER = true
				}
			}
			Expect(hasDOCKER).To(BeTrue())
		})
	})

	// ───────────────────── ensureIngress ─────────────────────

	Describe("ensureIngress", func() {
		It("creates an Ingress with correct rules", func() {
			localCtx := context.Background()
			mtName := "mt-ingress"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:             mirrorv1alpha1.ExposeTypeIngress,
						Host:             "resources.example.com",
						IngressClassName: "nginx",
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.ensureIngress(localCtx, mt, mtName+"-resources")).To(Succeed())

			ingress := &networkingv1.Ingress{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName + "-resources", Namespace: ns}, ingress)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, ingress) })

			Expect(ingress.Spec.Rules).To(HaveLen(1))
			Expect(ingress.Spec.Rules[0].Host).To(Equal("resources.example.com"))
			Expect(*ingress.Spec.IngressClassName).To(Equal("nginx"))
		})

		It("returns error when host is empty", func() {
			localCtx := context.Background()
			mtName := "mt-ingress-nohost"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: mirrorv1alpha1.ExposeTypeIngress},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.ensureIngress(localCtx, mt, mtName+"-resources")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("host"))
		})
	})

	// ───────────────────── reconcileExposure ─────────────────────

	Describe("reconcileExposure", func() {
		It("returns nil for explicit Service type", func() {
			localCtx := context.Background()
			mtName := "mt-expose-svc"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: mirrorv1alpha1.ExposeTypeService},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileExposure(localCtx, mt)).To(Succeed())
		})

		It("returns nil for GatewayAPI type (not yet implemented)", func() {
			localCtx := context.Background()
			mtName := "mt-expose-gw"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: mirrorv1alpha1.ExposeTypeGatewayAPI},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileExposure(localCtx, mt)).To(Succeed())
		})

		It("creates Ingress when expose type is Ingress", func() {
			localCtx := context.Background()
			mtName := "mt-expose-ing"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type: mirrorv1alpha1.ExposeTypeIngress,
						Host: "resources.example.com",
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileExposure(localCtx, mt)).To(Succeed())

			ingress := &networkingv1.Ingress{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName + "-resources", Namespace: ns}, ingress)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, ingress) })
		})
	})

	// ───────────────────── handleDeletion edge cases ─────────────────────

	Describe("handleDeletion edge cases", func() {
		It("returns immediately when finalizer is not present", func() {
			localCtx := context.Background()
			mtName := "mt-del-nofin"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			// Simulate deletion without finalizer
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			// No finalizer on this MirrorTarget

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.handleDeletion(localCtx, mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("removes finalizer when no pods remain", func() {
			localCtx := context.Background()
			mtName := "mt-del-nopods"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			// Add finalizer
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
			Expect(k8sClient.Update(localCtx, mt)).To(Succeed())

			// Delete (sets DeletionTimestamp)
			Expect(k8sClient.Delete(localCtx, mt)).To(Succeed())

			// Re-read to get deletion timestamp
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			Expect(mt.DeletionTimestamp).NotTo(BeNil())

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.handleDeletion(localCtx, mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Finalizer should be removed
			fresh := &mirrorv1alpha1.MirrorTarget{}
			err = k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, fresh)
			// Object might already be fully deleted since finalizer was removed
			if err == nil {
				Expect(controllerutil.ContainsFinalizer(fresh, mirrorTargetFinalizer)).To(BeFalse())
			}
		})
	})

	// ───────────────────── MirrorTarget Reconcile with ImageSets ─────────────────────

	Describe("MirrorTarget Reconcile with ImageSets", func() {
		It("aggregates ImageSet status and updates KnownImageSets", func() {
			localCtx := context.Background()
			mtName := "mt-aggregate-cov"
			isName := "is-for-aggregate-cov"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			is.Status.TotalImages = 10
			is.Status.MirroredImages = 5
			is.Status.PendingImages = 3
			is.Status.FailedImages = 2
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{isName},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// First reconcile: adds finalizer
			_, err := r.Reconcile(localCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mtName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: full reconciliation with aggregation
			_, err = r.Reconcile(localCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mtName, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			Expect(mt.Status.KnownImageSets).To(ContainElement(isName))
			Expect(mt.Status.TotalImages).To(Equal(10))
			Expect(mt.Status.MirroredImages).To(Equal(5))
			Expect(mt.Status.PendingImages).To(Equal(3))
			Expect(mt.Status.FailedImages).To(Equal(2))
		})
	})

	// ───────────────────── MirrorTarget Reconcile - not found ─────────────────────

	Describe("MirrorTarget Reconcile not found", func() {
		It("returns no error for non-existent MirrorTarget", func() {
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent-mt-cov", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})
})
