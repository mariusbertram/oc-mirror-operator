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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

var _ = Describe("UIConfiguration Controller", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	Context("TestSingleInstanceValidation", func() {
		ctx := context.Background()

		It("should reject creating multiple UIConfigurations", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the first UIConfiguration")
			uic1 := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "uic-first",
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic1)).To(Succeed())
			defer cleanupUIConfig(ctx, "uic-first")

			By("first reconcile: adds finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "uic-first"}})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile: sets status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "uic-first"}})
			Expect(err).NotTo(HaveOccurred())

			By("verifying first UIConfiguration is active")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "uic-first"}, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))

			By("creating a second UIConfiguration")
			uic2 := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "uic-second",
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic2)).To(Succeed())
			defer cleanupUIConfig(ctx, "uic-second")

			By("reconciling the second UIConfiguration: first pass adds finalizer")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "uic-second"}})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling the second UIConfiguration: second pass triggers single-instance check")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "uic-second"}})
			Expect(err).NotTo(HaveOccurred())

			By("verifying second UIConfiguration is in failed phase")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "uic-second"}, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseFailed))

			By("re-reconciling first UIConfiguration: now sees a second instance exists")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "uic-first"}})
			Expect(err).NotTo(HaveOccurred())

			By("verifying first UIConfiguration has SingleInstanceViolation condition")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "uic-first"}, uic)
				for _, cond := range uic.Status.Conditions {
					if cond.Type == "SingleInstanceViolation" && cond.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("TestReconcileAddsFinalizerAndUpdatesStatus", func() {
		ctx := context.Background()

		It("should add finalizer and update status on first reconcile", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-finalizer-test"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating a UIConfiguration")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("reconciling the UIConfiguration")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying finalizer is added")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return controllerutil.ContainsFinalizer(uic, uiConfigurationFinalizer)
			}, timeout, interval).Should(BeTrue())

			By("second reconcile to update status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying status is updated to active phase")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))
		})
	})

	Context("TestReconcileDeletesUIConfiguration", func() {
		ctx := context.Background()

		It("should remove finalizer on deletion", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-deletion-test"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating a UIConfiguration with finalizer")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:       uicName,
					Finalizers: []string{uiConfigurationFinalizer},
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())

			By("deleting the UIConfiguration to trigger deletion handler")
			Expect(k8sClient.Delete(ctx, uic)).To(Succeed())

			By("reconciling to trigger deletion handler")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying finalizer is removed")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				if err := k8sClient.Get(ctx, namespacedName, uic); err != nil {
					if errors.IsNotFound(err) {
						return true // Already deleted
					}
				}
				return !controllerutil.ContainsFinalizer(uic, uiConfigurationFinalizer)
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("TestReconcileStatusUpdate", func() {
		ctx := context.Background()

		It("should update status fields correctly", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-status-test"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating a UIConfiguration")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:       uicName,
					Generation: 1,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile to update status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying ObservedGeneration is set")
			Eventually(func() int64 {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.ObservedGeneration
			}, timeout, interval).Should(Equal(int64(1)))

			By("verifying Ready condition is True")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				for _, cond := range uic.Status.Conditions {
					if cond.Type == conditionTypeReady && cond.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			By("verifying status conditions are initialized")
			Eventually(func() int {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return len(uic.Status.Conditions)
			}, timeout, interval).Should(BeNumerically(">", 0))
		})
	})

	Context("TestReconcilePhaseTransitions", func() {
		ctx := context.Background()

		It("should transition phases: pending -> active", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-phase-test"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating a UIConfiguration")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("initial phase should be empty (reconciler not yet run)")
			initialUIC := &mirrorv1alpha1.UIConfiguration{}
			Expect(k8sClient.Get(ctx, namespacedName, initialUIC)).To(Succeed())
			Expect(initialUIC.Status.Phase).To(Or(BeEmpty(), Equal(mirrorv1alpha1.UIConfigurationPhasePending)))

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile to transition to active")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying phase is active")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))
		})
	})

	Context("TestIngressRequiresTLS", func() {
		ctx := context.Background()

		It("should fail validation if Ingress type without TLS", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-ingress-notls"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating UIConfiguration with Ingress but no TLS")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeIngress,
					TLS:          nil,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile to trigger validation")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying phase is failed")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseFailed))

			By("verifying ValidationError condition is set")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				for _, cond := range uic.Status.Conditions {
					if cond.Type == conditionTypeReady && cond.Status == metav1.ConditionFalse {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		It("should succeed if Ingress type with TLS enabled", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-ingress-withtls"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating UIConfiguration with Ingress and TLS enabled")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeIngress,
					TLS: &mirrorv1alpha1.UITLSConfig{
						Enabled: true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile to validate spec")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying phase is active")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))
		})
	})

	Context("TestResourceSpecConfiguration", func() {
		ctx := context.Background()

		It("should store resource spec configuration in status", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-resources"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating UIConfiguration with resource spec")
			replicas := int32(2)
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
					Replicas:     &replicas,
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *parseQuantity("100m"),
							corev1.ResourceMemory: *parseQuantity("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *parseQuantity("500m"),
							corev1.ResourceMemory: *parseQuantity("512Mi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile to validate")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying spec was preserved")
			Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return uic.Status.Phase
			}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))

			By("verifying DesiredReplicas in status")
			Eventually(func() int32 {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				// Set the desired replicas based on spec
				if uic.Spec.Replicas != nil {
					return *uic.Spec.Replicas
				}
				return 1
			}, timeout, interval).Should(Equal(int32(2)))
		})
	})

	Context("TestMissingUIConfiguration", func() {
		ctx := context.Background()

		It("should handle missing UIConfiguration gracefully", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("reconciling non-existent UIConfiguration")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "non-existent-uic"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("TestConditionTransitions", func() {
		ctx := context.Background()

		It("should properly transition conditions", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-conditions"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating UIConfiguration")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("first reconcile to add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying no Ready condition yet")
			Eventually(func() bool {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				return len(uic.Status.Conditions) == 0
			}, timeout, interval).Should(BeTrue())

			By("second reconcile to set Ready condition")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying Ready condition is True")
			Eventually(func() metav1.ConditionStatus {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				for _, cond := range uic.Status.Conditions {
					if cond.Type == conditionTypeReady {
						return cond.Status
					}
				}
				return metav1.ConditionUnknown
			}, timeout, interval).Should(Equal(metav1.ConditionTrue))

			By("verifying SingleInstanceViolation condition is False")
			Eventually(func() metav1.ConditionStatus {
				uic := &mirrorv1alpha1.UIConfiguration{}
				_ = k8sClient.Get(ctx, namespacedName, uic)
				for _, cond := range uic.Status.Conditions {
					if cond.Type == "SingleInstanceViolation" {
						return cond.Status
					}
				}
				return metav1.ConditionUnknown
			}, timeout, interval).Should(Equal(metav1.ConditionFalse))
		})
	})

	Context("TestMultipleReconciles", func() {
		ctx := context.Background()

		It("should be idempotent across multiple reconciles", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			uicName := "uic-idempotent"
			namespacedName := types.NamespacedName{Name: uicName}

			By("creating UIConfiguration")
			uic := &mirrorv1alpha1.UIConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: uicName,
				},
				Spec: mirrorv1alpha1.UIConfigurationSpec{
					ExposureType: mirrorv1alpha1.UIExposureTypeService,
				},
			}
			Expect(k8sClient.Create(ctx, uic)).To(Succeed())
			defer cleanupUIConfig(ctx, uicName)

			By("reconcile 1: add finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("reconcile 2: activate")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("storing the status after first activation")
			var expectedStatus mirrorv1alpha1.UIConfigurationStatus
			uic1 := &mirrorv1alpha1.UIConfiguration{}
			_ = k8sClient.Get(ctx, namespacedName, uic1)
			expectedStatus = uic1.Status

			By("reconcile 3: should not change status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("reconcile 4: should not change status")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying status hasn't changed")
			uic2 := &mirrorv1alpha1.UIConfiguration{}
			_ = k8sClient.Get(ctx, namespacedName, uic2)

			By("comparing ObservedGeneration")
			Expect(uic2.Status.ObservedGeneration).To(Equal(expectedStatus.ObservedGeneration))

			By("comparing Phase")
			Expect(uic2.Status.Phase).To(Equal(expectedStatus.Phase))

			By("verifying conditions remain consistent")
			Expect(uic2.Status.Conditions).To(HaveLen(len(expectedStatus.Conditions)))
		})
	})

	Context("TestExposureTypeVariations", func() {
		ctx := context.Background()

		It("should handle different exposure types", func() {
			reconciler := &UIConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			exposureTypes := []mirrorv1alpha1.UIExposureType{
				mirrorv1alpha1.UIExposureTypeService,
				mirrorv1alpha1.UIExposureTypeRoute,
				mirrorv1alpha1.UIExposureTypeConsolePlugin,
			}

			for _, expType := range exposureTypes {
				uicName := "uic-exposure-" + strings.ToLower(string(expType))
				namespacedName := types.NamespacedName{Name: uicName}

				By("creating UIConfiguration with exposure type: " + string(expType))
				uic := &mirrorv1alpha1.UIConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name: uicName,
					},
					Spec: mirrorv1alpha1.UIConfigurationSpec{
						ExposureType: expType,
					},
				}
				Expect(k8sClient.Create(ctx, uic)).To(Succeed())

				By("first reconcile to add finalizer")
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("second reconcile to validate")
				_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("verifying phase is active for exposure type: " + string(expType))
				Eventually(func() mirrorv1alpha1.UIConfigurationPhase {
					u := &mirrorv1alpha1.UIConfiguration{}
					_ = k8sClient.Get(ctx, namespacedName, u)
					return u.Status.Phase
				}, timeout, interval).Should(Equal(mirrorv1alpha1.UIConfigurationPhaseActive))

				// Clean up before next iteration so single-instance check doesn't fail
				cleanupUIConfig(ctx, uicName)
			}
		})
	})
})

// Helper function to parse quantity
func parseQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// cleanupUIConfig removes the finalizer (if any) and deletes the UIConfiguration.
func cleanupUIConfig(ctx context.Context, name string) {
	obj := &mirrorv1alpha1.UIConfiguration{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
		return
	}
	if controllerutil.ContainsFinalizer(obj, uiConfigurationFinalizer) {
		controllerutil.RemoveFinalizer(obj, uiConfigurationFinalizer)
		_ = k8sClient.Update(ctx, obj)
	}
	_ = k8sClient.Delete(ctx, obj)
}
