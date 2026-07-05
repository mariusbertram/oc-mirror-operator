/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	exportbuilder "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/export/builder"
)

func newMirrorExportReconciler() *MirrorExportReconciler {
	Expect(os.Setenv("OPERATOR_IMAGE", "test-controller:latest")).To(Succeed())
	mgr, err := exportbuilder.New()
	Expect(err).NotTo(HaveOccurred())
	return &MirrorExportReconciler{
		Client:         k8sClient,
		Scheme:         k8sClient.Scheme(),
		ExportBuildMgr: mgr,
	}
}

// deleteMirrorExportAndDerived removes a MirrorExport plus every resource the
// reconciler creates for it. envtest does not run a garbage collector
// controller, so owned objects are not cleaned up automatically when the
// owner is deleted — each test must do so explicitly to avoid leaking
// deterministically-named objects into the next test.
func deleteMirrorExportAndDerived(ctx context.Context, name string) {
	me := &mirrorv1alpha1.MirrorExport{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, me); err == nil {
		_ = k8sClient.Delete(ctx, me)
	}

	saName := exportServiceAccountName(name)
	_ = k8sClient.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ArtifactsConfigMapName(name), Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: exportbuilder.JobName(name), Namespace: "default"}})
}

var _ = Describe("MirrorExport Controller", func() {
	const (
		meTimeout  = 30 * time.Second
		meInterval = 250 * time.Millisecond
	)

	Context("Happy path", func() {
		const exportName = "me-happy"
		ctx := context.Background()
		namespacedName := types.NamespacedName{Name: exportName, Namespace: "default"}

		BeforeEach(func() {
			me := &mirrorv1alpha1.MirrorExport{
				ObjectMeta: metav1.ObjectMeta{Name: exportName, Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorExportSpec{
					Mirror: mirrorv1alpha1.Mirror{
						AdditionalImages: []mirrorv1alpha1.AdditionalImage{{Name: "quay.io/foo/bar:v1"}},
					},
					Destination: mirrorv1alpha1.MirrorExportDestination{Registry: "registry.example.com/mirror"},
				},
			}
			Expect(k8sClient.Create(ctx, me)).To(Succeed())
		})

		AfterEach(func() {
			deleteMirrorExportAndDerived(ctx, exportName)
		})

		It("creates RBAC, an artifacts ConfigMap, and an export build Job", func() {
			reconciler := newMirrorExportReconciler()
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			saName := exportServiceAccountName(exportName)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, &corev1.ServiceAccount{})).To(Succeed())

			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, role)).To(Succeed())
			Expect(role.Rules).To(HaveLen(1))
			Expect(role.Rules[0].ResourceNames).To(ConsistOf(ArtifactsConfigMapName(exportName)))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, &rbacv1.RoleBinding{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ArtifactsConfigMapName(exportName), Namespace: "default"}, &corev1.ConfigMap{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: exportbuilder.JobName(exportName), Namespace: "default"}, &batchv1.Job{})).To(Succeed())

			me := &mirrorv1alpha1.MirrorExport{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, namespacedName, me); err != nil {
					return false
				}
				for _, c := range me.Status.Conditions {
					if c.Type == conditionTypeReady && c.Status == metav1.ConditionFalse && c.Reason == "ExportBuildRunning" {
						return true
					}
				}
				return false
			}, meTimeout, meInterval).Should(BeTrue())
		})

		It("marks Ready=True and records TotalImages once the Job succeeds", func() {
			reconciler := newMirrorExportReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Simulate export-builder having populated the artifacts ConfigMap
			// and the Job completing successfully.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ArtifactsConfigMapName(exportName), Namespace: "default"}, cm)).To(Succeed())
			cm.Data = map[string]string{"manifest.json": `{"images":[{"source":"a","destination":"b"}]}`}
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: exportbuilder.JobName(exportName), Namespace: "default"}, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			me := &mirrorv1alpha1.MirrorExport{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, namespacedName, me); err != nil {
					return false
				}
				if me.Status.TotalImages != 1 || me.Status.ArtifactsConfigMap != ArtifactsConfigMapName(exportName) {
					return false
				}
				for _, c := range me.Status.Conditions {
					if c.Type == conditionTypeReady && c.Status == metav1.ConditionTrue && c.Reason == "Rendered" {
						return true
					}
				}
				return false
			}, meTimeout, meInterval).Should(BeTrue())
		})
	})

	Context("When the MirrorExport no longer exists", func() {
		It("returns no error", func() {
			reconciler := newMirrorExportReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"}})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
