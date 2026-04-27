/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"math/rand"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

var _ = Describe("MirrorTarget Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("Reconcile", func() {
		var (
			resourceName   string
			namespacedName types.NamespacedName
		)

		BeforeEach(func() {
			_ = os.Setenv("OPERATOR_IMAGE", "test-image:latest")
			resourceName = "mt-" + randString(5)
			namespacedName = types.NamespacedName{Name: resourceName, Namespace: "default"}

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "quay.io/oc-mirror",
					ImageSets: []string{"test-imageset"},
				},
			}
			Expect(k8sClient.Create(ctx, mt)).To(Succeed())
		})

		It("should delete worker pods and remove finalizer on deletion", func() {
			reconciler := &MirrorTargetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("first reconcile: adds the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			mt := &mirrorv1alpha1.MirrorTarget{}
			Expect(k8sClient.Get(ctx, namespacedName, mt)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer)).To(BeTrue())

			By("creating a fake worker pod labelled with the MirrorTarget name")
			podName := types.NamespacedName{Name: "worker-pod-" + resourceName, Namespace: "default"}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName.Name,
					Namespace: "default",
					Labels: map[string]string{
						"app":          "oc-mirror-worker",
						"mirrortarget": resourceName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			By("deleting the MirrorTarget (sets DeletionTimestamp)")
			Expect(k8sClient.Delete(ctx, mt)).To(Succeed())

			By("first reconcile: deletes the worker pod and requeues")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the worker pod was deleted")
			Eventually(func() bool {
				p := &corev1.Pod{}
				return errors.IsNotFound(k8sClient.Get(ctx, podName, p))
			}, timeout, interval).Should(BeTrue())

			By("second reconcile (after pods are gone): removes the finalizer")
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
				if err != nil {
					return false
				}
				p := &mirrorv1alpha1.MirrorTarget{}
				err = k8sClient.Get(ctx, namespacedName, p)
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		})
	})
})

func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
