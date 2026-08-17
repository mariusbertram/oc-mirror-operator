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

	// envtest's API server does not have the OpenShift console.openshift.io
	// ConsolePlugin CRD installed (see suite_test.go), so Reconcile's CRD-gate
	// (r.RESTMapper().RESTMapping(...)) can only ever observe "unavailable"
	// there — see "Reconcile edge cases" above. These tests instead use a fake
	// client seeded with a RESTMapper that knows about the ConsolePlugin GVK to
	// exercise the actual happy paths.
	Context("Reconcile and ensureConsolePlugin with the ConsolePlugin CRD available (fake RESTMapper)", func() {
		consolePluginGVK := schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsolePlugin"}

		newFakeReconciler := func() (*ConsolePluginReconciler, client.Client) {
			fakeScheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(corev1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(rbacv1.AddToScheme(fakeScheme)).To(Succeed())

			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{consolePluginGVK.GroupVersion()})
			rm.Add(consolePluginGVK, apimeta.RESTScopeRoot)

			c := fake.NewClientBuilder().WithScheme(fakeScheme).WithRESTMapper(rm).Build()
			return &ConsolePluginReconciler{
				Client:      c,
				Scheme:      fakeScheme,
				Namespace:   testNamespace,
				PluginImage: testPluginImage,
			}, c
		}

		It("creates every namespace-scoped resource plus the ConsolePlugin CR with its cleanup finalizer", func() {
			r, c := newFakeReconciler()
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pluginReconcileInterval))

			Expect(c.Get(ctx, types.NamespacedName{Name: pluginSAName, Namespace: testNamespace}, &corev1.ServiceAccount{})).To(Succeed())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginRoleName, Namespace: testNamespace}, &rbacv1.Role{})).To(Succeed())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginRoleName, Namespace: testNamespace}, &rbacv1.RoleBinding{})).To(Succeed())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, &appsv1.Deployment{})).To(Succeed())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginServiceName, Namespace: testNamespace}, &corev1.Service{})).To(Succeed())

			plugin := &unstructured.Unstructured{}
			plugin.SetGroupVersionKind(consolePluginGVK)
			Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(plugin, pluginCleanupFinalizer)).To(BeTrue())

			displayName, _, _ := unstructured.NestedString(plugin.Object, "spec", "displayName")
			Expect(displayName).To(Equal("OC Mirror Operator"))
			svcName, _, _ := unstructured.NestedString(plugin.Object, "spec", "backend", "service", "name")
			Expect(svcName).To(Equal(pluginServiceName))

			// Second reconcile is idempotent.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cleans up namespace-scoped resources and removes the finalizer when the ConsolePlugin CR is deleted", func() {
			r, c := newFakeReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())

			plugin := &unstructured.Unstructured{}
			plugin.SetGroupVersionKind(consolePluginGVK)
			Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(Succeed())
			Expect(c.Delete(ctx, plugin)).To(Succeed())

			// The fake client honors finalizers: Delete only stamps a
			// DeletionTimestamp while pluginCleanupFinalizer is still present.
			Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(Succeed())
			Expect(plugin.GetDeletionTimestamp().IsZero()).To(BeFalse())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())

			Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, &appsv1.Deployment{})).To(HaveOccurred())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginServiceName, Namespace: testNamespace}, &corev1.Service{})).To(HaveOccurred())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginSAName, Namespace: testNamespace}, &corev1.ServiceAccount{})).To(HaveOccurred())

			// The CR itself is now gone (no finalizer left to hold it back).
			Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(HaveOccurred())
		})
	})
})
