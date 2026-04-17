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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
)

func newImageSetReconciler() *ImageSetReconciler {
	mc := mirrorclient.NewMirrorClient(nil, "")
	return &ImageSetReconciler{
		Client:       k8sClient,
		Scheme:       k8sClient.Scheme(),
		MirrorClient: mc,
		Collector:    mirror.NewCollector(mc),
		StateManager: state.New(mc),
	}
}

var _ = Describe("ImageSet Controller", func() {
	const (
		isTimeout  = 30 * time.Second
		isInterval = 250 * time.Millisecond
	)

	Context("When MirrorTarget does not exist", func() {
		const resourceName = "is-notarget"
		ctx := context.Background()
		namespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}

		BeforeEach(func() {
			By("creating an ImageSet referencing a nonexistent MirrorTarget")
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					TargetRef: "nonexistent",
					Mirror:    mirrorv1alpha1.Mirror{},
				},
			}
			Expect(k8sClient.Create(ctx, is)).To(Succeed())
		})

		AfterEach(func() {
			is := &mirrorv1alpha1.ImageSet{}
			if err := k8sClient.Get(ctx, namespacedName, is); err == nil {
				_ = k8sClient.Delete(ctx, is)
			}
		})

		It("should return no error and set Ready=False with MirrorTargetNotFound", func() {
			reconciler := newImageSetReconciler()

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			is := &mirrorv1alpha1.ImageSet{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, namespacedName, is); err != nil {
					return false
				}
				for _, c := range is.Status.Conditions {
					if c.Type == "Ready" && c.Status == metav1.ConditionFalse && c.Reason == "MirrorTargetNotFound" {
						return true
					}
				}
				return false
			}, isTimeout, isInterval).Should(BeTrue())
		})
	})

	Context("Happy path", func() {
		const (
			mtName = "mt-for-imageset"
			isName = "is-happy"
		)
		ctx := context.Background()
		mtNamespacedName := types.NamespacedName{Name: mtName, Namespace: "default"}
		isNamespacedName := types.NamespacedName{Name: isName, Namespace: "default"}

		BeforeEach(func() {
			By("creating the MirrorTarget")
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/mirror",
				},
			}
			Expect(k8sClient.Create(ctx, mt)).To(Succeed())

			By("creating the ImageSet referencing the MirrorTarget")
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					TargetRef: mtName,
					Mirror:    mirrorv1alpha1.Mirror{},
				},
			}
			Expect(k8sClient.Create(ctx, is)).To(Succeed())
		})

		AfterEach(func() {
			is := &mirrorv1alpha1.ImageSet{}
			if err := k8sClient.Get(ctx, isNamespacedName, is); err == nil {
				_ = k8sClient.Delete(ctx, is)
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			if err := k8sClient.Get(ctx, mtNamespacedName, mt); err == nil {
				_ = k8sClient.Delete(ctx, mt)
			}
		})

		It("should set ObservedGeneration equal to Generation and set a Ready condition", func() {
			reconciler := newImageSetReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: isNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			is := &mirrorv1alpha1.ImageSet{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, isNamespacedName, is); err != nil {
					return false
				}
				return is.Status.ObservedGeneration > 0
			}, isTimeout, isInterval).Should(BeTrue())

			Expect(is.Status.ObservedGeneration).To(Equal(is.Generation))
			Expect(is.Status.Conditions).NotTo(BeEmpty())
		})
	})

	Context("Re-collect on spec change", func() {
		const (
			mtName = "mt-for-recollect"
			isName = "is-recollect"
		)
		ctx := context.Background()
		mtNamespacedName := types.NamespacedName{Name: mtName, Namespace: "default"}
		isNamespacedName := types.NamespacedName{Name: isName, Namespace: "default"}

		BeforeEach(func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/mirror",
				},
			}
			Expect(k8sClient.Create(ctx, mt)).To(Succeed())

			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.ImageSetSpec{
					TargetRef: mtName,
					Mirror:    mirrorv1alpha1.Mirror{},
				},
			}
			Expect(k8sClient.Create(ctx, is)).To(Succeed())
		})

		AfterEach(func() {
			is := &mirrorv1alpha1.ImageSet{}
			if err := k8sClient.Get(ctx, isNamespacedName, is); err == nil {
				_ = k8sClient.Delete(ctx, is)
			}
			mt := &mirrorv1alpha1.MirrorTarget{}
			if err := k8sClient.Get(ctx, mtNamespacedName, mt); err == nil {
				_ = k8sClient.Delete(ctx, mt)
			}
		})

		It("should update ObservedGeneration after the spec changes", func() {
			reconciler := newImageSetReconciler()

			By("initial reconcile")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: isNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			is := &mirrorv1alpha1.ImageSet{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, isNamespacedName, is); err != nil {
					return false
				}
				return is.Status.ObservedGeneration > 0
			}, isTimeout, isInterval).Should(BeTrue())
			initialObservedGen := is.Status.ObservedGeneration

			By("updating the ImageSet spec to bump its generation")
			is.Spec.Mirror.AdditionalImages = []mirrorv1alpha1.AdditionalImage{
				{Name: "quay.io/example/image:latest"},
			}
			Expect(k8sClient.Update(ctx, is)).To(Succeed())

			By("reconciling again after spec change")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: isNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &mirrorv1alpha1.ImageSet{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, isNamespacedName, updated); err != nil {
					return false
				}
				return updated.Status.ObservedGeneration > initialObservedGen
			}, isTimeout, isInterval).Should(BeTrue())
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
		})
	})
})
