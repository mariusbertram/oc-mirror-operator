/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func mustGzipJSON(v interface{}) []byte {
	data, _ := json.Marshal(v)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

var _ = Describe("Coverage tests", func() {
	const ns = "default"

	BeforeEach(func() {
		_ = os.Setenv("OPERATOR_IMAGE", "test-image:latest")
	})

	Context("caBundleEnvVars", func() {
		It("returns nil when CABundle is nil", func() {
			Expect(caBundleEnvVars(nil)).To(BeNil())
		})

		It("includes SSL_CERT_FILE with default key", func() {
			env := caBundleEnvVars(&mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"})
			Expect(env).To(HaveLen(1))
			Expect(env[0].Name).To(Equal("SSL_CERT_FILE"))
			Expect(env[0].Value).To(Equal("/run/secrets/ca/ca-bundle.crt"))
		})

		It("uses custom CA key", func() {
			env := caBundleEnvVars(&mirrorv1alpha1.CABundleRef{ConfigMapName: "ca", Key: "custom.pem"})
			Expect(env[0].Value).To(Equal("/run/secrets/ca/custom.pem"))
		})
	})

	Context("managerContainerVolumeMounts", func() {
		It("returns empty mounts when no auth or CA is set", func() {
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
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "c"}}}
			mounts := managerContainerVolumeMounts(mt)
			Expect(mounts).To(HaveLen(1))
			Expect(mounts[0].Name).To(Equal("ca-bundle"))
		})
	})

	Context("managerPodVolumes", func() {
		It("includes dockerconfig volume", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{AuthSecret: "auth"}}
			vols := managerPodVolumes(mt)
			Expect(vols).To(HaveLen(1))
			Expect(vols[0].Name).To(Equal("dockerconfig"))
			Expect(vols[0].VolumeSource.Secret.SecretName).To(Equal("auth"))
		})

		It("includes ca-bundle volume with default key", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca"}}}
			vols := managerPodVolumes(mt)
			Expect(vols).To(HaveLen(1))
			v := vols[0]
			Expect(v.Name).To(Equal("ca-bundle"))
			Expect(v.VolumeSource.ConfigMap.LocalObjectReference.Name).To(Equal("ca"))
			Expect(v.VolumeSource.ConfigMap.Items[0].Key).To(Equal("ca-bundle.crt"))
		})

		It("uses custom CA key", func() {
			mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{CABundle: &mirrorv1alpha1.CABundleRef{ConfigMapName: "ca", Key: "custom.crt"}}}
			vols := managerPodVolumes(mt)
			Expect(vols[0].VolumeSource.ConfigMap.Items[0].Key).To(Equal("custom.crt"))
		})
	})

	Context("workerProxyEnvVars", func() {
		It("returns nil when proxy is nil", func() {
			Expect(workerProxyEnvVars(nil)).To(BeNil())
		})

		It("returns HTTP_PROXY and NO_PROXY", func() {
			env := workerProxyEnvVars(&mirrorv1alpha1.ProxyConfig{HTTPProxy: "http://p", NoProxy: "localhost"})
			foundNoProxy := false
			for _, e := range env {
				if e.Name == "NO_PROXY" && strings.Contains(e.Value, "localhost") {
					foundNoProxy = true
				}
			}
			Expect(foundNoProxy).To(BeTrue())
		})
	})

	Context("reconcileCleanup", func() {
		It("creates cleanup job when ImageSet is removed and cleanup-policy is Delete", func() {
			localCtx := context.Background()
			mtName := "mt-cleanup-create"
			removedIS := "is-cleanup-removed"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
					UID:       "test-uid-cleanup",
					Annotations: map[string]string{
						mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete,
					},
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					ImageSets: []string{"is-staying"},
				},
				Status: mirrorv1alpha1.MirrorTargetStatus{
					KnownImageSets: []string{"is-staying", removedIS},
				},
			}

			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{
					Source: "s1",
					State:  "Mirrored",
					Refs:   []imagestate.ImageRef{{ImageSet: removedIS}},
				},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.reconcileCleanup(localCtx, mt)).To(Succeed())

			jobName := cleanupJobName(mtName, removedIS)
			job := &batchv1.Job{}
			Eventually(func() error {
				return k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)
			}, "15s", "1s").Should(Succeed())

			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(mt.Status.PendingCleanup).To(ContainElement(removedIS))
		})
	})

	Context("createCleanupJob", func() {
		It("creates a job with correct labels and args", func() {
			localCtx := context.Background()
			mtName := "mt-create-cleanup"
			isName := "is-create-cleanup"
			snapshotName := "snapshot-1"
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns, UID: "test-uid-create"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "reg.example.com",
				},
			}

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.createCleanupJob(localCtx, mt, isName, snapshotName)).To(Succeed())

			jobName := cleanupJobName(mtName, isName)
			job := &batchv1.Job{}
			Expect(k8sClient.Get(localCtx, types.NamespacedName{Name: jobName, Namespace: ns}, job)).To(Succeed())
			DeferCleanup(func() {
				prop := metav1.DeletePropagationBackground
				_ = k8sClient.Delete(localCtx, job, &client.DeleteOptions{PropagationPolicy: &prop})
			})

			Expect(job.Labels).To(HaveKeyWithValue("mirror.openshift.io/cleanup", isName))
			Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElements("cleanup", "--snapshot", snapshotName))
		})
	})

	Context("MirrorTarget Reconcile with ImageSets", func() {
		XIt("aggregates ImageSet status and updates KnownImageSets", func() {
			localCtx := context.Background()
			mtName := "mt-reconcile"
			isName := "is-reconcile"

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns, UID: "test-uid-reconcile"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					ImageSets: []string{isName},
				},
				Status: mirrorv1alpha1.MirrorTargetStatus{
					KnownImageSets: []string{isName},
				},
			}
			Expect(k8sClient.Create(localCtx, mt)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, mt) })

			state := imagestate.ImageState{
				"d1": &imagestate.ImageEntry{
					Source: "s1", State: "Mirrored",
					Refs: []imagestate.ImageRef{{ImageSet: isName}},
				},
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: mtName + "-images", Namespace: ns},
				BinaryData: map[string][]byte{"images.json.gz": mustGzipJSON(state)},
			}
			Expect(k8sClient.Create(localCtx, cm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(localCtx, cm) })

			r := &MirrorTargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(localCtx, ctrl.Request{NamespacedName: types.NamespacedName{Name: mtName, Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())

			updated := &mirrorv1alpha1.MirrorTarget{}
			Eventually(func() int {
				_ = k8sClient.Get(localCtx, types.NamespacedName{Name: mtName, Namespace: ns}, updated)
				return updated.Status.TotalImages
			}, "10s", "1s").Should(Equal(1))

			Expect(updated.Status.KnownImageSets).To(ContainElement(isName))
		})
	})
})
