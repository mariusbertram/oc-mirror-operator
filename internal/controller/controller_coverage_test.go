package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

		// Regression: a freshly added operator entry has no imagestate entries
		// yet — the state still reflects the OLD spec (all Mirrored). The gate
		// must NOT report complete until the manager has resolved the new entry,
		// otherwise the catalog build launches before its bundle images exist.
		It("returns (false, true) when a spec operator entry has no state entries yet", func() {
			oldOp := mirrorv1alpha1.Operator{Catalog: "quay.io/old/catalog:v1"}
			newOp := mirrorv1alpha1.Operator{Catalog: "quay.io/new/catalog:v1"}
			oldSig := mirrorv1alpha1.OperatorEntrySignature(oldOp)

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: oldSig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "spec-drift-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "spec-drift", Namespace: ns},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{oldOp, newOp},
				}},
			}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeFalse())
			Expect(know).To(BeTrue())
		})

		It("returns (true, true) when every spec operator entry is resolved and Mirrored", func() {
			op := mirrorv1alpha1.Operator{Catalog: "quay.io/ops/catalog:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: sig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "spec-synced-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "spec-synced", Namespace: ns},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{op},
				}},
			}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeTrue())
			Expect(know).To(BeTrue())
		})

		It("returns (false, true) when ObservedGeneration lags the current spec Generation, even if every known entry is Mirrored", func() {
			op := mirrorv1alpha1.Operator{Catalog: "quay.io/gen-lag/catalog:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: sig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "gen-lag-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "gen-lag", Namespace: ns, Generation: 2},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{op},
				}},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
			}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeFalse())
			Expect(know).To(BeTrue())
		})

		It("returns (true, true) when ObservedGeneration matches the current spec Generation and every entry is Mirrored", func() {
			op := mirrorv1alpha1.Operator{Catalog: "quay.io/gen-match/catalog:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: sig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "gen-match-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "gen-match", Namespace: ns, Generation: 2},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{op},
				}},
				Status: mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 2},
			}
			complete, know := operatorImagesMirrored(bgCtx, c, is)
			Expect(complete).To(BeTrue())
			Expect(know).To(BeTrue())
		})

		It("skips signature enforcement when legacy entries without EntrySig exist", func() {
			op := mirrorv1alpha1.Operator{Catalog: "quay.io/legacy/catalog:v1"}

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-state-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cm).Build()
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-state", Namespace: ns},
				Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{op},
				}},
			}
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
				if c.Type == conditionCatalogReady && c.Reason == reasonWaitingForOperatorMirror {
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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			op := mirrorv1alpha1.Operator{
				Catalog: "quay.io/redhat/catalog:v4.21",
				IncludeConfig: mirrorv1alpha1.IncludeConfig{
					Packages: []mirrorv1alpha1.IncludePackage{{Name: "web-terminal"}},
				},
			}
			// pinnedCatalogRef requires a resolved-digest annotation (written
			// by the manager once it has actually mirrored images for this
			// entry) before a CatalogBuildJob will be created — this is the
			// digest-pinning gate that stops a build from ever pulling
			// content the manager hasn't mirrored yet.
			digestAnnoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation: "",
						digestAnnoKey:                      mirrorv1alpha1.OperatorCacheValue("sha256:abc123"),
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{op},
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

		It("pins the CatalogBuildJob's pull target to the resolved digest, not the mutable tag", func() {
			// Regression test: op.Catalog is a mutable tag. Without pinning,
			// a CatalogBuildJob re-resolves that tag independently of the
			// manager, at whatever moment the Job pod actually runs — which
			// can land on a NEWER upstream digest than what the manager last
			// mirrored, producing a catalog that advertises operator bundle
			// versions never actually pushed to the target registry. The Job
			// must instead pull the exact digest recorded in the manager's
			// own resolved-digest annotation.
			localCtx := context.Background()
			isName := "is-catbuild-pin"
			mtName := "mt-catbuild-pin"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			op := mirrorv1alpha1.Operator{Catalog: "quay.io/redhat/pincat:v4.21"}
			digestAnnoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))
			resolvedDigest := "sha256:" + strings.Repeat("a", 64)

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation: "",
						digestAnnoKey:                      mirrorv1alpha1.OperatorCacheValue(resolvedDigest),
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{Operators: []mirrorv1alpha1.Operator{op}},
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

			// The Job is still identified/tracked by the raw tag reference
			// (stable across digest churn)...
			jobName := builder.JobName(isName, "quay.io/redhat/pincat:v4.21")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			// ...but what it actually pulls (SOURCE_CATALOG) must be pinned
			// to the resolved digest, never the raw mutable tag.
			var sourceCatalog string
			for _, e := range job.Spec.Template.Spec.Containers[0].Env {
				if e.Name == builder.EnvSourceCatalog {
					sourceCatalog = e.Value
				}
			}
			Expect(sourceCatalog).To(Equal("quay.io/redhat/pincat@" + resolvedDigest))
		})

		It("defers catalog build when the gate is open but no resolved digest is recorded yet", func() {
			// Even when recollect (or any other gate) opens the door to a
			// build, EnsureCatalogBuildJob must never run against an
			// unpinned tag — if the manager hasn't recorded a resolved
			// digest for this entry yet, the build has to wait rather than
			// guess at upstream content.
			localCtx := context.Background()
			isName := "is-catbuild-nodigest"
			mtName := "mt-catbuild-nodigest"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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
						Operators: []mirrorv1alpha1.Operator{{Catalog: "quay.io/redhat/nodigest:v4.21"}},
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

			jobName := builder.JobName(isName, "quay.io/redhat/nodigest:v4.21")
			j := &batchv1.Job{}
			err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j)
			Expect(err).To(HaveOccurred(), "no job should be created without a resolved digest to pin to")

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundDeferred bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == reasonWaitingForOperatorMirror {
					foundDeferred = true
				}
			}
			Expect(foundDeferred).To(BeTrue(), "expected CatalogReady=WaitingForOperatorMirror")
		})

		It("sets CatalogReady=True when all jobs succeeded", func() {
			localCtx := context.Background()
			isName := "is-catbuild-succeed"
			mtName := "mt-catbuild-succeed"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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

			// Real callers only ever pass pollExpired=true when
			// Status.LastSuccessfulPollTime is set and stale; set a realistic
			// stale timestamp so the new poll-rebuild dedup (which compares
			// against this value) does not mistake this for the zero-value case.
			is.Status.LastSuccessfulPollTime = &metav1.Time{Time: time.Now().Add(-48 * time.Hour)}
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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

		It("defers rebuild when sig changed but operator images still pending (alreadyBuilt bypass bug)", func() {
			localCtx := context.Background()
			isName := "is-rebuild-gate"
			mtName := "mt-rebuild-gate"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			// Catalog was previously built successfully (CatalogReady=True) with
			// the old sig "old-sig". The upstream catalog digest changed, so the
			// manager produced new bundle images in Pending state and the
			// annotation now has the stale sig. A rebuild would be triggered by
			// the sig mismatch — but the new images are not yet mirrored.
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						"mirror.openshift.io/catalog-build-sig": "old-sig",
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "quay.io/test/rebuildgate:v1"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// Set CatalogReady=True (simulates catalog was built in a prior reconcile).
			is.Status.Conditions = []metav1.Condition{
				{Type: conditionCatalogReady, Status: metav1.ConditionTrue, Reason: "CatalogBuildSucceeded",
					Message: "ok", LastTransitionTime: metav1.Now()},
			}
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			// Write imagestate with one operator image still Pending — mirroring incomplete.
			pendingState := imagestate.ImageState{
				"reg.example.com/bundle:latest": {
					Source: "quay.io/bundle:latest",
					State:  "Pending",
					Origin: imagestate.OriginOperator,
				},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(pendingState)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			r := &ImageSetReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				CatalogBuildMgr: bm,
			}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			// No build job must have been created — rebuild is gated on images being mirrored.
			jobName := builder.JobName(isName, "quay.io/test/rebuildgate:v1")
			j := &batchv1.Job{}
			err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j)
			Expect(err).To(HaveOccurred(), "rebuild job must not be created while images are still pending")

			// The condition must reflect the deferred state, not CatalogReady=True.
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundDeferred bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == reasonWaitingForOperatorMirror {
					foundDeferred = true
				}
			}
			Expect(foundDeferred).To(BeTrue(), "expected CatalogReady=WaitingForOperatorMirror while rebuild gated")
		})

		It("keeps the gate open when a build job already exists, even without recollect or mirrored images", func() {
			localCtx := context.Background()
			isName := "is-catgate-jobrace"
			mtName := "mt-catgate-jobrace"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			// An operator entry with an empty Catalog is skipped by every loop
			// that walks is.Spec.Mirror.Operators — include one to exercise
			// those "continue" branches alongside the real entry below.
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{},
							{Catalog: "quay.io/redhat/jobrace:v1"},
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			// No recollect annotation and no imagestate ConfigMap — the gate
			// would normally stay closed — but a CatalogBuildJob already
			// exists (Pending by default), which must keep it open so a
			// build already in flight is never abandoned mid-run.
			jobName := builder.JobName(isName, "quay.io/redhat/jobrace:v1")
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "build", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogBuildMgr: bm}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundRunning bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == "CatalogBuildRunning" {
					foundRunning = true
				}
			}
			Expect(foundRunning).To(BeTrue(), "expected CatalogReady=CatalogBuildRunning once the existing job kept the gate open")
		})

		It("logs the mirroring-still-in-progress reason (not the no-imagestate reason) when state is known but incomplete", func() {
			localCtx := context.Background()
			isName := "is-catgate-knownstate"
			mtName := "mt-catgate-knownstate"

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
						Operators: []mirrorv1alpha1.Operator{{Catalog: "quay.io/redhat/knownstate:v1"}},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Pending", Origin: imagestate.OriginOperator},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			var foundDeferred bool
			for _, c := range is.Status.Conditions {
				if c.Type == conditionCatalogReady && c.Reason == reasonWaitingForOperatorMirror {
					foundDeferred = true
				}
			}
			Expect(foundDeferred).To(BeTrue(), "expected CatalogReady=WaitingForOperatorMirror")
		})

		It("only honors a recollect annotation once per distinct value", func() {
			localCtx := context.Background()
			isName := "is-catgate-recollect-once"
			mtName := "mt-catgate-recollect-once"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
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
					Name:      isName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.RecollectAnnotation: "run-1",
					},
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{{Catalog: "quay.io/redhat/recollectonce:v1"}},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogBuildMgr: bm}
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, false)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(is.Annotations["mirror.openshift.io/catalog-build-recollect-sig"]).To(Equal("run-1"),
				"a fresh recollect value must be recorded as honored so it is not re-applied every reconcile")

			DeferCleanup(func() {
				jobName := builder.JobName(isName, "quay.io/redhat/recollectonce:v1")
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})

		It("forces a rebuild via poll expiry even with nil annotations, initializing the annotation map", func() {
			localCtx := context.Background()
			isName := "is-catgate-pollnilanno"
			mtName := "mt-catgate-pollnilanno"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			op := mirrorv1alpha1.Operator{Catalog: "quay.io/redhat/pollnilanno:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			// No annotations at all — gate must open via operatorMirroringComplete,
			// not recollect, and the persisted-signature step must tolerate a nil
			// Annotations map.
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{Operators: []mirrorv1alpha1.Operator{op}}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: sig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			is.Status.LastSuccessfulPollTime = &metav1.Time{Time: time.Now().Add(-48 * time.Hour)}
			// Simulate the manager having already cleanly resolved this exact
			// spec generation (see operatorImagesMirrored's ObservedGeneration
			// guard) — otherwise the gate correctly stays closed regardless of
			// what the imagestate ConfigMap says.
			is.Status.ObservedGeneration = is.Generation
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogBuildMgr: bm}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, true)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(is.Annotations["mirror.openshift.io/catalog-build-poll-sig"]).NotTo(BeEmpty(),
				"poll-forced rebuild must record the poll marker it honored, even starting from a nil annotation map")

			DeferCleanup(func() {
				jobName := builder.JobName(isName, "quay.io/redhat/pollnilanno:v1")
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})

		It("does not force a poll-expiry rebuild while a build job is still Pending or Running", func() {
			localCtx := context.Background()
			isName := "is-catgate-pollbusy"
			mtName := "mt-catgate-pollbusy"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())
			bm, bmErr := builder.New()
			Expect(bmErr).NotTo(HaveOccurred())

			op := mirrorv1alpha1.Operator{Catalog: "quay.io/redhat/pollbusy:v1"}
			sig := mirrorv1alpha1.OperatorEntrySignature(op)
			buildSig := bm.BuildSignature([]mirrorv1alpha1.Operator{op})

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
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
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{{}, op},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginOperator, EntrySig: sig},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			// A build job for this catalog is already active (default status,
			// i.e. Pending) — the poll-expiry rebuild must not be forced while
			// it is in flight.
			jobName := builder.JobName(isName, "quay.io/redhat/pollbusy:v1")
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "build", Image: "busybox"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			is.Status.LastSuccessfulPollTime = &metav1.Time{Time: time.Now().Add(-48 * time.Hour)}
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			r := &ImageSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), CatalogBuildMgr: bm}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(r.reconcileCatalogBuildJobs(localCtx, is, mt, true)).To(Succeed())

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName, Namespace: ns}, is)).To(Succeed())
			Expect(is.Annotations["mirror.openshift.io/catalog-build-poll-sig"]).To(BeEmpty(),
				"poll-forced rebuild must not fire while the existing build job is still Pending/Running")
		})
	})

	// ───────────────────── ImageSet Reconcile: poll interval handling ─────────────────────

	Describe("ImageSet Reconcile poll interval handling", func() {
		// Note: Reconcile's own clamp of a sub-1h PollInterval up to the 1h
		// floor (mirrortarget_controller.go's PollInterval < 1h check) is not
		// exercised here — the MirrorTarget CRD's validation schema already
		// rejects any pollInterval between 0 (exclusive) and 1h (exclusive),
		// so a real MirrorTarget with that shape cannot exist in envtest. The
		// in-code clamp is defense-in-depth for objects that predate the CRD
		// validation being added; it is not reachable through the validated API.

		It("computes pollExpired and publishes the last-poll gauge when LastSuccessfulPollTime is set", func() {
			localCtx := context.Background()
			isName := "is-poll-expired"
			mtName := "mt-poll-expired"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			is.Status.LastSuccessfulPollTime = &metav1.Time{Time: time.Now().Add(-48 * time.Hour)}
			Expect(k8sClient.Status().Update(localCtx, is)).To(Succeed())

			r := newImageSetReconciler()
			result, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: isName, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(24 * time.Hour))
		})

		It("returns a bare empty result when polling is explicitly disabled", func() {
			localCtx := context.Background()
			isName := "is-poll-disabled"
			mtName := "mt-poll-disabled"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:     "reg.example.com",
					ImageSets:    []string{isName},
					PollInterval: &metav1.Duration{Duration: 0},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			r := newImageSetReconciler()
			result, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: isName, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("returns the underlying error for a non-NotFound Get failure", func() {
			localCtx := context.Background()
			isName := "is-poll-getcancel"

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: isName, Namespace: ns},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			Expect(k8sClient.Create(localCtx, is)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, is) })

			cancelledCtx, cancel := context.WithCancel(context.Background())
			cancel()

			r := newImageSetReconciler()
			_, err := r.Reconcile(cancelledCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: isName, Namespace: ns}})
			Expect(err).To(HaveOccurred())
		})
	})

	// ───────────────────── pinnedCatalogRef ─────────────────────

	Describe("pinnedCatalogRef", func() {
		It("returns ok=false when the catalog reference cannot be parsed for digest-pinning", func() {
			op := mirrorv1alpha1.Operator{Catalog: "not a valid image reference!!"}
			digestAnnoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "is-pin-invalid",
					Annotations: map[string]string{
						digestAnnoKey: mirrorv1alpha1.OperatorCacheValue("sha256:" + strings.Repeat("a", 64)),
					},
				},
			}
			_, ok := pinnedCatalogRef(is, op)
			Expect(ok).To(BeFalse())
		})
	})

	// ───────────────────── reconcileCleanup ─────────────────────

	Describe("reconcileCleanup", func() {
		It("creates cleanup job when ImageSet is removed and cleanup-policy is Delete", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-create"
			removedIS := "is-cleanup-removed"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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

			// Create the removed ImageSet's own state ConfigMap with its exclusive
			// images (no shared-image index entry — exclusive to removedIS).
			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginAdditional},
			}
			removedISCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: removedIS + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, removedISCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, removedISCM) })

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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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

		It("does not delete orphaned images when cleanup-policy is not Delete", func() {
			// Regression test: images the manager moves into the pending-orphans
			// ConfigMap (spec narrowing/blocking — e.g. a catalog's heads-only
			// channel selection advancing to a newer bundle version) must
			// respect the same cleanup-policy=Delete gate as a fully removed
			// ImageSet. Without the gate, these images get deleted from the
			// registry (and immediately re-mirrored under a new destination)
			// on every routine re-resolve, even for MirrorTargets that never
			// opted into registry deletion.
			localCtx := context.Background()
			mtName := "mt-orphans-nopolicy"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "reg.example.com",
					ImageSets: []string{"is-orphans-nopolicy"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			mt.Status.KnownImageSets = []string{"is-orphans-nopolicy"}

			orphans := imagestate.ImageState{
				"d-orphan": {Source: "s-orphan", State: "Mirrored", Origin: imagestate.OriginOperator},
			}
			orphansCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(orphans)},
			}
			Expect(k8sClient.Create(localCtx, orphansCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, orphansCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			// No cleanup job for the orphans should have been created.
			jobName := cleanupJobName(mtName, "orphans")
			j := &batchv1.Job{}
			err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j)
			Expect(err).To(HaveOccurred())
			Expect(mt.Status.PendingCleanup).NotTo(ContainElement("orphans"))

			// The pending-orphans ConfigMap is left untouched — nothing was
			// deleted, so there is nothing to hand off to a cleanup Job.
			stillThere := &corev1.ConfigMap{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns}, stillThere)).To(Succeed())
		})

		It("creates cleanup job for orphaned images when cleanup-policy is Delete", func() {
			localCtx := context.Background()
			mtName := "mt-orphans-delete"

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
					ImageSets: []string{"is-orphans-delete"},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			mt.Status.KnownImageSets = []string{"is-orphans-delete"}

			orphans := imagestate.ImageState{
				"d-orphan2": {Source: "s-orphan2", State: "Mirrored", Origin: imagestate.OriginOperator},
			}
			orphansCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(orphans)},
			}
			Expect(k8sClient.Create(localCtx, orphansCM)).To(Succeed())

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			jobName := cleanupJobName(mtName, "orphans")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})
			Expect(mt.Status.PendingCleanup).To(ContainElement("orphans"))

			snapshotName := cleanupSnapshotCMName(mtName, "orphans")
			snapshotCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: snapshotName, Namespace: ns}, snapshotCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, snapshotCM) })
		})
	})

	// ───────────────────── createCleanupJob ─────────────────────

	Describe("createCleanupJob", func() {
		It("creates a job with correct labels and args", func() {
			localCtx := context.Background()
			mtName := "mt-create-cleanup"
			isName := "is-create-cleanup"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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

	// ───────────────────── ensureHTTPRoute ─────────────────────

	Describe("ensureHTTPRoute", func() {
		It("returns error when gatewayRef is not set", func() {
			localCtx := context.Background()
			mtName := "mt-httproute-nogw"

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
			err := r.ensureHTTPRoute(localCtx, mt, mtName+"-resources")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gatewayRef"))
		})

		It("returns error when gatewayRef.name is empty", func() {
			localCtx := context.Background()
			mtName := "mt-httproute-emptygw"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						GatewayRef: &mirrorv1alpha1.GatewayReference{},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.ensureHTTPRoute(localCtx, mt, mtName+"-resources")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gatewayRef"))
		})

		It("returns error when the Gateway API CRD is not installed", func() {
			localCtx := context.Background()
			mtName := "mt-httproute-nocrd"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						GatewayRef: &mirrorv1alpha1.GatewayReference{Name: "my-gateway", Namespace: "other-ns"},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.ensureHTTPRoute(localCtx, mt, mtName+"-resources")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Gateway API"))
		})
	})

	// ───────────────────── hasGatewayAPI ─────────────────────

	Describe("hasGatewayAPI", func() {
		It("returns false when the Gateway API CRD is not installed in the test cluster", func() {
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.hasGatewayAPI(context.Background())).To(BeFalse())
		})
	})

	// ───────────────────── deleteHTTPRoute ─────────────────────

	Describe("deleteHTTPRoute", func() {
		It("is a no-op when no HTTPRoute exists", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-delete-httproute-noop", Namespace: ns},
			}
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(func() { r.deleteHTTPRoute(context.Background(), mt) }).NotTo(Panic())
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

		It("returns an error for GatewayAPI type when the Gateway API CRD is not installed", func() {
			localCtx := context.Background()
			mtName := "mt-expose-gw"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						GatewayRef: &mirrorv1alpha1.GatewayReference{Name: "my-gateway"},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			// The envtest API server does not have the Gateway API CRD installed,
			// matching the OpenShift-Route behavior (also untestable against envtest
			// without installing the foreign CRD) — this exercises the same
			// CRD-discovery guard as hasRouteAPI.
			err := r.reconcileExposure(localCtx, mt)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Gateway API"))
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

		It("requeues without removing the finalizer while a pod still has a DeletionTimestamp", func() {
			localCtx := context.Background()
			mtName := "mt-del-podterminating"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
			Expect(k8sClient.Update(localCtx, mt)).To(Succeed())
			Expect(k8sClient.Delete(localCtx, mt)).To(Succeed())
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())

			// A pod carrying a finalizer of its own so Delete only stamps a
			// DeletionTimestamp instead of removing it outright — simulating a
			// worker pod still terminating.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "worker-terminating-" + mtName,
					Namespace:  ns,
					Labels:     map[string]string{"mirrortarget": mtName},
					Finalizers: []string{"mirror.openshift.io/test-block-deletion"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(localCtx, pod)).To(Succeed())
			Expect(k8sClient.Delete(localCtx, pod)).To(Succeed())
			DeferCleanup(func() {
				fresh := &corev1.Pod{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: pod.Name, Namespace: ns}, fresh); err == nil {
					controllerutil.RemoveFinalizer(fresh, "mirror.openshift.io/test-block-deletion")
					_ = k8sClient.Update(localCtx, fresh)
					_ = k8sClient.Delete(localCtx, fresh)
				}
			})

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.handleDeletion(localCtx, mt)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{RequeueAfter: 5 * time.Second}))

			// Finalizer must still be present — cleanup is not yet complete.
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer)).To(BeTrue())
		})

		It("returns the underlying error when listing pods fails", func() {
			localCtx := context.Background()
			mtName := "mt-del-listcancel"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())
			controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
			Expect(k8sClient.Update(localCtx, mt)).To(Succeed())
			Expect(k8sClient.Delete(localCtx, mt)).To(Succeed())
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, mt)).To(Succeed())

			cancelledCtx, cancel := context.WithCancel(localCtx)
			cancel()

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.handleDeletion(cancelledCtx, mt)
			Expect(err).To(HaveOccurred())
		})
	})

	// ───────────────────── Reconcile: error propagation from sub-reconcilers ─────────────────────

	Describe("Reconcile error propagation from sub-reconcilers", func() {
		It("surfaces a reconcileCleanup failure as Cleanup=False/CleanupError and returns the error", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-cleanuperr"
			removedIS := "is-reconcile-cleanuperr"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			// First reconcile only adds the finalizer.
			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Seed KnownImageSets with an ImageSet no longer in spec.imageSets
			// (which is empty), and give it a corrupt state ConfigMap so
			// reconcileCleanup's removed-ImageSet path fails to load it.
			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			mt.Status.KnownImageSets = []string{removedIS}
			Expect(k8sClient.Status().Update(localCtx, mt)).To(Succeed())

			corruptCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: removedIS + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": []byte("not-a-gzip-stream")},
			}
			Expect(k8sClient.Create(localCtx, corruptCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, corruptCM) })

			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeCleanup && c.Status == metav1.ConditionFalse && c.Reason == "CleanupError" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Cleanup=False/CleanupError")
		})

		It("surfaces an ensureCoordinatorRBAC failure as Ready=False/ReconcileError and returns the error", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-rbacerr"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Pre-create the coordinator ServiceAccount owned by a different
			// controller so SetControllerReference (inside ensureCoordinatorRBAC)
			// refuses to adopt it.
			otherOwner := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-other-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, otherOwner)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, otherOwner) })

			conflictSA := &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-coordinator",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "mirror.openshift.io/v1alpha1",
							Kind:       "MirrorTarget",
							Name:       otherOwner.Name,
							UID:        otherOwner.UID,
							Controller: pointerTo(true),
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, conflictSA)).To(Succeed())

			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == reasonReconcileError {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Ready=False/ReconcileError")
		})

		It("wraps the error when the worker ServiceAccount is already owned by a different controller", func() {
			localCtx := context.Background()
			mtName := "mt-coordrbac-workersa-conflict"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			otherOwner := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-workersa-other-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, otherOwner)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, otherOwner) })

			conflictSA := &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-worker",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "mirror.openshift.io/v1alpha1",
							Kind:       "MirrorTarget",
							Name:       otherOwner.Name,
							UID:        otherOwner.UID,
							Controller: pointerTo(true),
						},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, conflictSA)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, conflictSA) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.ensureCoordinatorRBAC(localCtx, mt)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to create worker ServiceAccount"))

			DeferCleanup(func() {
				_ = k8sClient.Delete(localCtx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: mtName + "-coordinator", Namespace: ns}})
			})
		})

		It("surfaces a manager Deployment CreateOrUpdate failure as Ready=False/ReconcileError and returns the error", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-deployerr"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			otherOwner := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-deploy-other-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, otherOwner)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, otherOwner) })

			conflictDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-manager",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "mirror.openshift.io/v1alpha1",
							Kind:       "MirrorTarget",
							Name:       otherOwner.Name,
							UID:        otherOwner.UID,
							Controller: pointerTo(true),
						},
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "placeholder"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "placeholder"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "placeholder", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, conflictDeployment)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, conflictDeployment) })

			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == reasonReconcileError {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Ready=False/ReconcileError")
		})

		It("surfaces a manager Service CreateOrUpdate failure as Ready=False/ReconcileError and returns the error", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-svcerr"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			// First reconcile adds the finalizer; second creates the Deployment
			// (succeeds normally) and then reaches the Service step.
			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			otherOwner := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-svc-other-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, otherOwner)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, otherOwner) })

			conflictService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-manager",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "mirror.openshift.io/v1alpha1",
							Kind:       "MirrorTarget",
							Name:       otherOwner.Name,
							UID:        otherOwner.UID,
							Controller: pointerTo(true),
						},
					},
				},
				Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
			}
			Expect(k8sClient.Create(localCtx, conflictService)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, conflictService) })

			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == reasonReconcileError {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Ready=False/ReconcileError")

			DeferCleanup(func() {
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: mtName + "-manager", Namespace: ns}, dep); err == nil {
					_ = k8sClient.Delete(localCtx, dep)
				}
			})
		})

		It("surfaces a resources Service CreateOrUpdate failure as Ready=False/ReconcileError and returns the error", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-ressvcerr"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			otherOwner := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-ressvc-other-owner", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, otherOwner)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, otherOwner) })

			conflictService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-resources",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "mirror.openshift.io/v1alpha1",
							Kind:       "MirrorTarget",
							Name:       otherOwner.Name,
							UID:        otherOwner.UID,
							Controller: pointerTo(true),
						},
					},
				},
				Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8081}}},
			}
			Expect(k8sClient.Create(localCtx, conflictService)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, conflictService) })

			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == reasonReconcileError {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Ready=False/ReconcileError")

			DeferCleanup(func() {
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: mtName + "-manager", Namespace: ns}, dep); err == nil {
					_ = k8sClient.Delete(localCtx, dep)
				}
				svc := &corev1.Service{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: mtName + "-manager", Namespace: ns}, svc); err == nil {
					_ = k8sClient.Delete(localCtx, svc)
				}
			})
		})

		It("sets Ready=False/ExposureError but still completes reconciliation when exposure fails", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-exposeerr"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					// envtest has no Gateway API CRD installed, so this always fails —
					// exercising the ExposureError branch which, unlike the other
					// sub-reconciler errors above, must NOT abort the rest of Reconcile.
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						GatewayRef: &mirrorv1alpha1.GatewayReference{Name: "my-gateway"},
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == "ExposureError" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Ready=False/ExposureError")
			// KnownImageSets must still have been advanced despite the exposure failure.
			Expect(mt.Status.KnownImageSets).To(BeEmpty())
		})

		It("requeues after 30s while a cleanup job is still pending", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile-pendingcleanup"
			removedIS := "is-reconcile-pendingcleanup"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: mtName, Namespace: ns}

			_, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(localCtx, key, mt)).To(Succeed())
			mt.Status.KnownImageSets = []string{removedIS}
			Expect(k8sClient.Status().Update(localCtx, mt)).To(Succeed())

			state := imagestate.ImageState{"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginAdditional}}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: removedIS + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())

			result, err := r.Reconcile(localCtx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{RequeueAfter: 30 * time.Second}))

			DeferCleanup(func() {
				snapshotName := cleanupSnapshotCMName(mtName, removedIS)
				_ = k8sClient.Delete(localCtx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: snapshotName, Namespace: ns}})
				jobName := cleanupJobName(mtName, removedIS)
				j := &batchv1.Job{}
				if err := k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, j); err == nil {
					prop := metav1.DeletePropagationBackground
					_ = k8sClient.Delete(localCtx, j, &client.DeleteOptions{PropagationPolicy: &prop})
				}
			})
		})
	})

	// ───────────────────── MirrorTarget Reconcile with ImageSets ─────────────────────

	Describe("MirrorTarget Reconcile with ImageSets", func() {
		It("aggregates ImageSet status and updates KnownImageSets", func() {
			localCtx := context.Background()
			mtName := "mt-aggregate-cov"
			isName := "is-for-aggregate-cov"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

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

	// ───────────────────── aggregateImageSetStatus extra edge cases ─────────────────────

	Describe("aggregateImageSetStatus extra edge cases", func() {
		It("derives totals from the deduplicated per-ImageSet imagestate once any state exists", func() {
			localCtx := context.Background()
			mtName := "mt-aggregate-merged"
			isName := "is-aggregate-merged"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{isName}},
			}

			state := imagestate.ImageState{
				"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginAdditional},
				"d2": {Source: "s2", State: "Pending", Origin: imagestate.OriginAdditional},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.aggregateImageSetStatus(localCtx, mt)).To(Succeed())

			Expect(mt.Status.TotalImages).To(Equal(2))
			Expect(mt.Status.MirroredImages).To(Equal(1))
			Expect(mt.Status.PendingImages).To(Equal(1))
		})

		It("returns the underlying error when listing ImageSets fails", func() {
			localCtx, cancel := context.WithCancel(context.Background())
			cancel()

			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: "mt-aggregate-listcancel", Namespace: ns}}
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			err := r.aggregateImageSetStatus(localCtx, mt)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("list ImageSets"))
		})
	})

	// ───────────────────── reconcileExposure: unrecognized expose type ─────────────────────

	Describe("reconcileExposure with an unrecognized expose type", func() {
		It("returns nil without creating any exposure object", func() {
			// This shape can only be constructed in-memory: the MirrorTarget CRD's
			// spec.expose.type enum rejects any value outside the known set at
			// admission, so this default branch is unreachable through the
			// validated API and is exercised directly here instead.
			localCtx := context.Background()
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-expose-unrecognized", Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: "SomethingElse"},
				},
			}
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileExposure(localCtx, mt)).To(Succeed())
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

		It("returns the underlying error for a non-NotFound Get failure", func() {
			mtName := "mt-getcancel-cov"
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(context.Background(), mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(context.Background(), mtName) })

			cancelledCtx, cancel := context.WithCancel(context.Background())
			cancel()

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(cancelledCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mtName, Namespace: ns},
			})
			Expect(err).To(HaveOccurred())
		})
	})

	// ───────────────────── isPendingCleanup ─────────────────────

	Describe("isPendingCleanup", func() {
		It("returns false when the job is gone and no snapshot ConfigMap remains", func() {
			localCtx := context.Background()
			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: "mt-pc-absent", Namespace: ns}}
			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.isPendingCleanup(localCtx, mt, "is-pc-absent")).To(BeFalse())
		})

		It("re-creates the job and returns true when the job is gone but the snapshot ConfigMap remains", func() {
			localCtx := context.Background()
			mtName := "mt-pc-resnap"
			isName := "is-pc-resnap"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			snapshotName := cleanupSnapshotCMName(mtName, isName)
			snapshotCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: snapshotName, Namespace: ns}}
			Expect(k8sClient.Create(localCtx, snapshotCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, snapshotCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.isPendingCleanup(localCtx, mt, isName)).To(BeTrue())

			jobName := cleanupJobName(mtName, isName)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})
		})
	})

	// ───────────────────── partitionAndCreateCleanupJob ─────────────────────

	Describe("partitionAndCreateCleanupJob", func() {
		It("returns an error when the ImageSet's own state ConfigMap is corrupt", func() {
			localCtx := context.Background()
			mtName := "mt-partition-loaderr"
			isName := "is-partition-loaderr"

			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns}}
			corruptCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": []byte("not-a-gzip-stream")},
			}
			Expect(k8sClient.Create(localCtx, corruptCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, corruptCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.partitionAndCreateCleanupJob(localCtx, mt, isName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to load state for removed imageset"))
		})

		It("returns an error when the shared image index ConfigMap is corrupt", func() {
			localCtx := context.Background()
			mtName := "mt-partition-indexerr"
			isName := "is-partition-indexerr"

			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns}}

			state := imagestate.ImageState{"d1": {Source: "s1", State: "Mirrored", Origin: imagestate.OriginAdditional}}
			isCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, isCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, isCM) })

			indexCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images-index", Namespace: ns},
				BinaryData: map[string][]byte{"index.json.gz": []byte("not-a-gzip-stream")},
			}
			Expect(k8sClient.Create(localCtx, indexCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, indexCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.partitionAndCreateCleanupJob(localCtx, mt, isName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to load shared image index"))
		})

		It("skips job creation and only updates the shared index when every image is shared with another ImageSet", func() {
			localCtx := context.Background()
			mtName := "mt-partition-allshared"
			isName := "is-partition-allshared"
			otherIS := "is-partition-allshared-other"

			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns}}

			state := imagestate.ImageState{"d-shared": {Source: "s-shared", State: "Mirrored", Origin: imagestate.OriginAdditional}}
			isCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, isCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, isCM) })

			idx := imagestate.SharedIndex{"d-shared": {isName, otherIS}}
			Expect(imagestate.SaveIndex(localCtx, k8sClient, ns, mtName, idx, nil, nil)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(localCtx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images-index", Namespace: ns}})
			})

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			created, err := r.partitionAndCreateCleanupJob(localCtx, mt, isName)
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(BeFalse())

			jobName := cleanupJobName(mtName, isName)
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, &batchv1.Job{})).To(HaveOccurred())

			gotIdx, err := imagestate.LoadIndex(localCtx, k8sClient, ns, mtName)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotIdx.Names("d-shared")).NotTo(ContainElement(isName))

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName + "-images", Namespace: ns}, &corev1.ConfigMap{})).To(HaveOccurred())
		})

		It("creates a cleanup job for exclusive images while updating the shared index for shared images", func() {
			localCtx := context.Background()
			mtName := "mt-partition-mixed"
			isName := "is-partition-mixed"
			otherIS := "is-partition-mixed-other"

			mt := &mirrorv1alpha1.MirrorTarget{ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns}}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			state := imagestate.ImageState{
				"d-exclusive": {Source: "s-exclusive", State: "Mirrored", Origin: imagestate.OriginAdditional},
				"d-shared":    {Source: "s-shared", State: "Mirrored", Origin: imagestate.OriginAdditional},
			}
			isCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: isName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, isCM)).To(Succeed())

			idx := imagestate.SharedIndex{"d-shared": {isName, otherIS}}
			Expect(imagestate.SaveIndex(localCtx, k8sClient, ns, mtName, idx, nil, nil)).To(Succeed())

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			created, err := r.partitionAndCreateCleanupJob(localCtx, mt, isName)
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(BeTrue())

			jobName := cleanupJobName(mtName, isName)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			snapshotName := cleanupSnapshotCMName(mtName, isName)
			snapshotCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: snapshotName, Namespace: ns}, snapshotCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, snapshotCM) })

			gotIdx, err := imagestate.LoadIndex(localCtx, k8sClient, ns, mtName)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotIdx.Names("d-shared")).NotTo(ContainElement(isName))
			DeferCleanup(func() {
				_ = k8sClient.Delete(localCtx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images-index", Namespace: ns}})
			})

			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: isName + "-images", Namespace: ns}, &corev1.ConfigMap{})).To(HaveOccurred())
		})
	})

	// ───────────────────── reconcileCleanup: CleanupComplete transition ─────────────────────

	Describe("reconcileCleanup CleanupComplete transition", func() {
		It("flips Cleanup to True/CleanupComplete once no removals or pending cleanups remain", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-complete"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com", ImageSets: []string{"is-still-present"}},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			mt.Status.KnownImageSets = []string{"is-still-present"}
			mt.Status.Conditions = []metav1.Condition{
				{Type: conditionTypeCleanup, Status: metav1.ConditionFalse, Reason: "CleanupInProgress",
					Message: "still cleaning up", LastTransitionTime: metav1.Now()},
			}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			var found bool
			for _, c := range mt.Status.Conditions {
				if c.Type == conditionTypeCleanup && c.Status == metav1.ConditionTrue && c.Reason == "CleanupComplete" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected Cleanup=True/CleanupComplete once removals/pending cleanups clear")
		})
	})

	// ───────────────────── reconcileRemovedImageSets: already-pending dedupe ─────────────────────

	Describe("reconcileRemovedImageSets already-pending dedupe", func() {
		It("does not re-attempt a cleanup job for an ImageSet already in PendingCleanup", func() {
			localCtx := context.Background()
			mtName := "mt-removed-alreadypending"
			isName := "is-removed-alreadypending"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
			}
			mt.Status.PendingCleanup = []string{isName}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileRemovedImageSets(localCtx, mt, []string{isName})).To(Succeed())

			Expect(mt.Status.PendingCleanup).To(Equal([]string{isName}))
			jobName := cleanupJobName(mtName, isName)
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, &batchv1.Job{})).To(HaveOccurred())
		})
	})

	// ───────────────────── reconcileOrphans: extra edge cases ─────────────────────

	Describe("reconcileOrphans extra edge cases", func() {
		It("is a no-op when an orphans cleanup is already pending", func() {
			localCtx := context.Background()
			mtName := "mt-orphans-alreadypending"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
			}
			mt.Status.PendingCleanup = []string{"orphans"}

			orphans := imagestate.ImageState{"d-orphan": {Source: "s-orphan", State: "Mirrored", Origin: imagestate.OriginOperator}}
			orphansCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(orphans)},
			}
			Expect(k8sClient.Create(localCtx, orphansCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, orphansCM) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileOrphans(localCtx, mt)).To(Succeed())

			Expect(mt.Status.PendingCleanup).To(Equal([]string{"orphans"}))
			// Left untouched — nothing was newly queued.
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns}, &corev1.ConfigMap{})).To(Succeed())
		})

		It("logs and leaves the orphans ConfigMap in place when createCleanupJob fails", func() {
			localCtx := context.Background()
			mtName := "mt-orphans-createfail"

			Expect(os.Setenv("OPERATOR_IMAGE", "test-operator:latest")).To(Succeed())
			Expect(os.Setenv("MANAGER_IMAGE", "test-manager:latest")).To(Succeed())
			Expect(os.Setenv("WORKER_IMAGE", "test-worker:latest")).To(Succeed())

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { cleanupMT(localCtx, mtName) })

			orphans := imagestate.ImageState{"d-orphan": {Source: "s-orphan", State: "Mirrored", Origin: imagestate.OriginOperator}}
			orphansCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(orphans)},
			}
			Expect(k8sClient.Create(localCtx, orphansCM)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, orphansCM) })

			// Pre-create a terminal (succeeded) cleanup job under the "orphans" name so
			// createCleanupJob takes its "delete stale job, error out to retry" branch.
			jobName := cleanupJobName(mtName, "orphans")
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

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileOrphans(localCtx, mt)).To(Succeed())

			Expect(mt.Status.PendingCleanup).NotTo(ContainElement("orphans"))
			snapshotName := cleanupSnapshotCMName(mtName, "orphans")
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: snapshotName, Namespace: ns}, &corev1.ConfigMap{})).To(HaveOccurred())
			// The source orphans ConfigMap is left in place for a retry on the next reconcile.
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: imagestate.OrphansConfigMapName(mtName), Namespace: ns}, &corev1.ConfigMap{})).To(Succeed())
		})
	})

	// ───────────────────── ensureRoute / hasRouteAPI / ensureHTTPRoute / hasGatewayAPI (fake RESTMapper) ─────────────────────
	//
	// envtest's API server does not have the OpenShift Route or Gateway API CRDs
	// installed (see suite_test.go), so the CRD-discovery ("does this API exist?")
	// branches of hasRouteAPI/hasGatewayAPI can only ever observe "unavailable"
	// there — see the "false" tests earlier in this file. To exercise the actual
	// happy paths (CRD present, object created/updated), these tests use a fake
	// client seeded with a RESTMapper that knows about the Route/HTTPRoute GVK,
	// mirroring the technique controller-runtime's own fake client is built for.
	Describe("ensureRoute and ensureHTTPRoute happy paths (fake RESTMapper)", func() {
		var fakeScheme *runtime.Scheme

		BeforeEach(func() {
			fakeScheme = runtime.NewScheme()
			Expect(mirrorv1alpha1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(corev1.AddToScheme(fakeScheme)).To(Succeed())
		})

		It("hasRouteAPI reports true and ensureRoute creates a Route with the given host", func() {
			routeGVK := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{routeGVK.GroupVersion()})
			rm.Add(routeGVK, apimeta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-fakeroute", Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: mirrorv1alpha1.ExposeTypeRoute, Host: "custom.example.com"},
				},
			}
			Expect(c.Create(context.Background(), mt)).To(Succeed())

			r := &MirrorTargetReconciler{Client: c, Scheme: fakeScheme}
			bgCtx := context.Background()
			Expect(r.hasRouteAPI(bgCtx)).To(BeTrue())

			svcName := "mt-fakeroute-resources"
			Expect(r.ensureRoute(bgCtx, mt, svcName)).To(Succeed())
			// Idempotent update path.
			Expect(r.ensureRoute(bgCtx, mt, svcName)).To(Succeed())

			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(routeGVK)
			Expect(c.Get(bgCtx, client.ObjectKey{Name: svcName, Namespace: ns}, route)).To(Succeed())

			to, found, err := unstructured.NestedString(route.Object, "spec", "to", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(to).To(Equal(svcName))

			host, found, err := unstructured.NestedString(route.Object, "spec", "host")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(host).To(Equal("custom.example.com"))

			Expect(route.GetOwnerReferences()).To(HaveLen(1))
		})

		It("ensureRoute omits spec.host when no host is configured", func() {
			routeGVK := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{routeGVK.GroupVersion()})
			rm.Add(routeGVK, apimeta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-fakeroute-nohost", Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose:   &mirrorv1alpha1.ExposeConfig{Type: mirrorv1alpha1.ExposeTypeRoute},
				},
			}
			Expect(c.Create(context.Background(), mt)).To(Succeed())

			r := &MirrorTargetReconciler{Client: c, Scheme: fakeScheme}
			bgCtx := context.Background()
			svcName := "mt-fakeroute-nohost-resources"
			Expect(r.ensureRoute(bgCtx, mt, svcName)).To(Succeed())

			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(routeGVK)
			Expect(c.Get(bgCtx, client.ObjectKey{Name: svcName, Namespace: ns}, route)).To(Succeed())
			_, found, err := unstructured.NestedString(route.Object, "spec", "host")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
		})

		It("reconcileExposure auto-detects Route when no expose type is set and the Route API is available", func() {
			routeGVK := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{routeGVK.GroupVersion()})
			rm.Add(routeGVK, apimeta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-fakeroute-auto", Namespace: ns},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.example.com"},
			}
			Expect(c.Create(context.Background(), mt)).To(Succeed())

			r := &MirrorTargetReconciler{Client: c, Scheme: fakeScheme}
			bgCtx := context.Background()
			Expect(r.reconcileExposure(bgCtx, mt)).To(Succeed())

			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(routeGVK)
			Expect(c.Get(bgCtx, client.ObjectKey{Name: "mt-fakeroute-auto-resources", Namespace: ns}, route)).To(Succeed())
		})

		It("hasGatewayAPI reports true and ensureHTTPRoute creates an HTTPRoute attached to the referenced Gateway", func() {
			httpRouteGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{httpRouteGVK.GroupVersion()})
			rm.Add(httpRouteGVK, apimeta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-fakehttproute", Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						Host:       "resources.example.com",
						GatewayRef: &mirrorv1alpha1.GatewayReference{Name: "my-gateway", Namespace: "gw-ns"},
					},
				},
			}
			Expect(c.Create(context.Background(), mt)).To(Succeed())

			r := &MirrorTargetReconciler{Client: c, Scheme: fakeScheme}
			bgCtx := context.Background()
			Expect(r.hasGatewayAPI(bgCtx)).To(BeTrue())

			svcName := "mt-fakehttproute-resources"
			Expect(r.ensureHTTPRoute(bgCtx, mt, svcName)).To(Succeed())
			// Idempotent update path.
			Expect(r.ensureHTTPRoute(bgCtx, mt, svcName)).To(Succeed())

			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(httpRouteGVK)
			Expect(c.Get(bgCtx, client.ObjectKey{Name: svcName, Namespace: ns}, hr)).To(Succeed())

			parentRefs, found, err := unstructured.NestedSlice(hr.Object, "spec", "parentRefs")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(parentRefs).To(HaveLen(1))
			parentRef, ok := parentRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(parentRef["name"]).To(Equal("my-gateway"))
			Expect(parentRef["namespace"]).To(Equal("gw-ns"))

			hostnames, found, err := unstructured.NestedStringSlice(hr.Object, "spec", "hostnames")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(hostnames).To(ConsistOf("resources.example.com"))
		})

		It("ensureHTTPRoute omits the Gateway namespace field when gatewayRef.namespace is unset", func() {
			httpRouteGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{httpRouteGVK.GroupVersion()})
			rm.Add(httpRouteGVK, apimeta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt-fakehttproute-samens", Namespace: ns},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
					Expose: &mirrorv1alpha1.ExposeConfig{
						Type:       mirrorv1alpha1.ExposeTypeGatewayAPI,
						GatewayRef: &mirrorv1alpha1.GatewayReference{Name: "same-ns-gateway"},
					},
				},
			}
			Expect(c.Create(context.Background(), mt)).To(Succeed())

			r := &MirrorTargetReconciler{Client: c, Scheme: fakeScheme}
			bgCtx := context.Background()
			svcName := "mt-fakehttproute-samens-resources"
			Expect(r.ensureHTTPRoute(bgCtx, mt, svcName)).To(Succeed())

			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(httpRouteGVK)
			Expect(c.Get(bgCtx, client.ObjectKey{Name: svcName, Namespace: ns}, hr)).To(Succeed())

			parentRefs, _, _ := unstructured.NestedSlice(hr.Object, "spec", "parentRefs")
			parentRef, ok := parentRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(parentRef).NotTo(HaveKey("namespace"))
		})
	})
})
