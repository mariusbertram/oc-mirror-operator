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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("ConsolePlugin Controller", func() {
	const (
		testNamespace   = "default"
		testPluginImage = "test-plugin:latest"
	)

	var (
		ctx        = context.Background()
		reconciler *ConsolePluginReconciler
	)

	BeforeEach(func() {
		reconciler = &ConsolePluginReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			Namespace:   testNamespace,
			PluginImage: testPluginImage,
		}
	})

	Context("ensureServiceAccount", func() {
		It("should create a ServiceAccount in the namespace", func() {
			By("calling ensureServiceAccount")
			Expect(reconciler.ensureServiceAccount(ctx)).To(Succeed())

			By("verifying the ServiceAccount exists")
			sa := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginSAName,
				Namespace: testNamespace,
			}, sa)).To(Succeed())
			Expect(sa.Name).To(Equal(pluginSAName))
		})
	})

	Context("ensureRBAC", func() {
		It("should create a Role and RoleBinding in the namespace", func() {
			By("calling ensureRBAC")
			Expect(reconciler.ensureRBAC(ctx)).To(Succeed())

			By("verifying the Role exists")
			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginRoleName,
				Namespace: testNamespace,
			}, role)).To(Succeed())
			Expect(role.Rules).NotTo(BeEmpty())

			By("verifying the RoleBinding exists")
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginRoleName,
				Namespace: testNamespace,
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(pluginRoleName))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal(pluginSAName))
		})
	})

	Context("ensureDeployment", func() {
		It("should create a Deployment with the correct container command", func() {
			By("calling ensureDeployment")
			Expect(reconciler.ensureDeployment(ctx)).To(Succeed())

			By("verifying the Deployment exists")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginDeploymentName,
				Namespace: testNamespace,
			}, dep)).To(Succeed())

			By("verifying the container command")
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := dep.Spec.Template.Spec.Containers[0]
			Expect(container.Command).To(Equal([]string{"/plugin"}))
			Expect(container.Image).To(Equal(testPluginImage))
		})
	})

	Context("ensureService", func() {
		It("should create a Service with the plugin port", func() {
			Expect(reconciler.ensureService(ctx)).To(Succeed())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginServiceName,
				Namespace: testNamespace,
			}, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(pluginPort))
		})

		It("should be idempotent on repeated calls", func() {
			Expect(reconciler.ensureService(ctx)).To(Succeed())
			Expect(reconciler.ensureService(ctx)).To(Succeed())
		})
	})

	Context("cleanupLegacyDashboard", func() {
		It("should not panic when legacy resources do not exist", func() {
			Expect(func() { reconciler.cleanupLegacyDashboard(ctx) }).NotTo(Panic())
		})

		It("should delete a pre-existing legacy dashboard Service", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      legacyDashboardServiceName,
					Namespace: testNamespace,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Port: 8080, Protocol: corev1.ProtocolTCP},
					},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			reconciler.cleanupLegacyDashboard(ctx)
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      legacyDashboardServiceName,
				Namespace: testNamespace,
			}, &corev1.Service{})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("deletePluginResources", func() {
		It("should not panic when resources do not exist", func() {
			Expect(func() { reconciler.deletePluginResources(ctx) }).NotTo(Panic())
		})

		It("should delete pre-created plugin resources", func() {
			Expect(reconciler.ensureServiceAccount(ctx)).To(Succeed())
			Expect(reconciler.ensureRBAC(ctx)).To(Succeed())
			Expect(reconciler.ensureDeployment(ctx)).To(Succeed())
			Expect(reconciler.ensureService(ctx)).To(Succeed())

			reconciler.deletePluginResources(ctx)

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginDeploymentName,
				Namespace: testNamespace,
			}, &appsv1.Deployment{})).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginServiceName,
				Namespace: testNamespace,
			}, &corev1.Service{})).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginSAName,
				Namespace: testNamespace,
			}, &corev1.ServiceAccount{})).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pluginRoleName,
				Namespace: testNamespace,
			}, &rbacv1.RoleBinding{})).To(HaveOccurred())
		})
	})

	Context("Reconcile edge cases", func() {
		It("returns RequeueAfter without error when PluginImage is not configured", func() {
			r := &ConsolePluginReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				Namespace:   testNamespace,
				PluginImage: "",
			}
			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pluginReconcileInterval))
		})

		It("returns RequeueAfter when ConsolePlugin CRD is unavailable in envtest", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pluginReconcileInterval))
		})
	})

	Context("enqueue helpers", func() {
		var r *ConsolePluginReconciler

		BeforeEach(func() {
			r = &ConsolePluginReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				Namespace:   "operator-ns",
				PluginImage: "img:latest",
			}
		})

		It("enqueueForPluginDeployment returns request for matching Deployment", func() {
			dep := &appsv1.Deployment{}
			dep.SetName(pluginDeploymentName)
			dep.SetNamespace("operator-ns")
			requests := r.enqueueForPluginDeployment(ctx, dep)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal("operator-ns"))
		})

		It("enqueueForPluginDeployment returns nil for wrong Deployment name", func() {
			dep := &appsv1.Deployment{}
			dep.SetName("unrelated")
			dep.SetNamespace("operator-ns")
			Expect(r.enqueueForPluginDeployment(ctx, dep)).To(BeNil())
		})

		It("enqueueForPluginDeployment returns nil for wrong namespace", func() {
			dep := &appsv1.Deployment{}
			dep.SetName(pluginDeploymentName)
			dep.SetNamespace("wrong-ns")
			Expect(r.enqueueForPluginDeployment(ctx, dep)).To(BeNil())
		})

		It("enqueueForConsolePlugin returns request for matching plugin name", func() {
			obj := &corev1.ConfigMap{}
			obj.SetName(consolePluginCRName)
			requests := r.enqueueForConsolePlugin(ctx, obj)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal("operator-ns"))
		})

		It("enqueueForConsolePlugin returns nil for non-matching name", func() {
			obj := &corev1.ConfigMap{}
			obj.SetName("some-other-plugin")
			Expect(r.enqueueForConsolePlugin(ctx, obj)).To(BeNil())
		})
	})

	Context("ensureConsolePlugin", func() {
		It("returns nil when ConsolePlugin CRD is not installed (envtest has no ConsolePlugin CRD)", func() {
			// The envtest API server doesn't have the ConsolePlugin CRD; ensureConsolePlugin
			// should catch the NoMatchError and return nil gracefully.
			Expect(reconciler.ensureConsolePlugin(ctx)).To(Succeed())
		})
	})

	Context("restrictedContainerSecurityContext", func() {
		It("returns a security context with AllowPrivilegeEscalation=false and ReadOnlyRootFilesystem=true", func() {
			sc := restrictedContainerSecurityContext()
			Expect(sc).NotTo(BeNil())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(sc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
			Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		})
	})
})
