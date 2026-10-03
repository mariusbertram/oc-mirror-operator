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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Namespace:    testNamespace,
			PluginImages: map[string]string{"4.20": testPluginImage},
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
			Expect(reconciler.ensureDeployment(ctx, testPluginImage)).To(Succeed())

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
			Expect(reconciler.deletePluginResources(ctx)).To(Succeed())
		})

		It("should delete pre-created plugin resources", func() {
			Expect(reconciler.ensureServiceAccount(ctx)).To(Succeed())
			Expect(reconciler.ensureRBAC(ctx)).To(Succeed())
			Expect(reconciler.ensureDeployment(ctx, testPluginImage)).To(Succeed())
			Expect(reconciler.ensureService(ctx)).To(Succeed())

			Expect(reconciler.deletePluginResources(ctx)).To(Succeed())

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
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Namespace:    "operator-ns",
				PluginImages: map[string]string{"4.20": "img:latest"},
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

		It("enqueueForConsoleClusterOperator returns request for the console operator", func() {
			obj := &unstructured.Unstructured{}
			obj.SetName(consoleClusterOperator)
			requests := r.enqueueForConsoleClusterOperator(ctx, obj)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal("operator-ns"))
		})

		It("enqueueForConsoleClusterOperator returns nil for another ClusterOperator", func() {
			obj := &unstructured.Unstructured{}
			obj.SetName("network")
			Expect(r.enqueueForConsoleClusterOperator(ctx, obj)).To(BeNil())
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

		It("watches Console ClusterOperator version changes and switches images without a manual reconcile", func() {
			// Use an isolated API server so OpenShift CRDs do not change the other
			// tests' deliberate non-OpenShift discovery behavior.
			env := &envtest.Environment{}
			for _, api := range []struct{ group, kind, plural string }{
				{"console.openshift.io", "ConsolePlugin", "consoleplugins"},
				{"config.openshift.io", "ClusterOperator", "clusteroperators"},
			} {
				preserve := true
				env.CRDs = append(env.CRDs, &apiextensionsv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{Name: api.plural + "." + api.group},
					Spec: apiextensionsv1.CustomResourceDefinitionSpec{
						Group: api.group,
						Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: api.kind, Plural: api.plural},
						Scope: apiextensionsv1.ClusterScoped,
						Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
							Name: "v1", Served: true, Storage: true,
							Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
								Type: "object", XPreserveUnknownFields: &preserve,
							}},
						}},
					},
				})
			}
			config, err := env.Start()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })
			mgr, err := ctrl.NewManager(config, ctrl.Options{
				Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			apiClient, err := client.New(config, client.Options{Scheme: mgr.GetScheme()})
			Expect(err).NotTo(HaveOccurred())
			namespace := "console-watch-test"
			Expect(apiClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
			r := &ConsolePluginReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Namespace: namespace,
				PluginImages: map[string]string{"4.18": "plugin:4.18", "4.22": "plugin:4.22"},
			}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
			managerCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- mgr.Start(managerCtx) }()
			DeferCleanup(func() {
				stop()
				Eventually(done, "15s").Should(Receive(BeNil()))
			})
			operator := &unstructured.Unstructured{}
			operator.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "ClusterOperator"})
			operator.SetName(consoleClusterOperator)
			setVersion := func(version string) {
				Expect(unstructured.SetNestedSlice(operator.Object, []interface{}{
					map[string]interface{}{"name": "operator", "version": version},
				}, "status", "versions")).To(Succeed())
			}
			setVersion("4.18.2")
			Expect(apiClient.Create(ctx, operator)).To(Succeed())
			image := func() string {
				deployment := &appsv1.Deployment{}
				if err := apiClient.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: namespace}, deployment); err != nil {
					return ""
				}
				return deployment.Spec.Template.Spec.Containers[0].Image
			}
			Eventually(image, "15s").Should(Equal("plugin:4.18"))
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(operator), operator)).To(Succeed())
			setVersion("4.22.1")
			Expect(apiClient.Update(ctx, operator)).To(Succeed())
			Eventually(image, "15s").Should(Equal("plugin:4.22"))
			Expect(apiClient.Delete(ctx, operator)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(apiClient.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: namespace}, &appsv1.Deployment{}))
			}, "15s").Should(BeTrue())
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
		clusterOperatorGVK := schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "ClusterOperator"}

		newFakeReconciler := func(version string, images map[string]string) (*ConsolePluginReconciler, client.WithWatch) {
			fakeScheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(corev1.AddToScheme(fakeScheme)).To(Succeed())
			Expect(rbacv1.AddToScheme(fakeScheme)).To(Succeed())

			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{
				consolePluginGVK.GroupVersion(),
				clusterOperatorGVK.GroupVersion(),
			})
			rm.Add(consolePluginGVK, apimeta.RESTScopeRoot)
			rm.Add(clusterOperatorGVK, apimeta.RESTScopeRoot)

			consoleOperator := &unstructured.Unstructured{}
			consoleOperator.SetGroupVersionKind(clusterOperatorGVK)
			consoleOperator.SetName(consoleClusterOperator)
			Expect(unstructured.SetNestedSlice(consoleOperator.Object, []interface{}{
				map[string]interface{}{"name": "network", "version": "4.99.0"},
				map[string]interface{}{"name": "operator", "version": version},
			}, "status", "versions")).To(Succeed())

			c := fake.NewClientBuilder().
				WithScheme(fakeScheme).
				WithRESTMapper(rm).
				WithObjects(consoleOperator).
				Build()
			return &ConsolePluginReconciler{
				Client:       c,
				Scheme:       fakeScheme,
				Namespace:    testNamespace,
				PluginImages: images,
			}, c
		}

		updateConsoleOperatorVersion := func(c client.Client, version string) {
			consoleOperator := &unstructured.Unstructured{}
			consoleOperator.SetGroupVersionKind(clusterOperatorGVK)
			Expect(c.Get(ctx, types.NamespacedName{Name: consoleClusterOperator}, consoleOperator)).To(Succeed())
			Expect(unstructured.SetNestedSlice(consoleOperator.Object, []interface{}{
				map[string]interface{}{"name": "operator", "version": version},
			}, "status", "versions")).To(Succeed())
			Expect(c.Update(ctx, consoleOperator)).To(Succeed())
		}

		expectUnregistered := func(c client.Client) {
			plugin := &unstructured.Unstructured{}
			plugin.SetGroupVersionKind(consolePluginGVK)
			Expect(apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin))).To(BeTrue())
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
				Expect(apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, obj))).To(BeTrue())
			}
		}

		It("creates every namespace-scoped resource plus the ConsolePlugin CR with its cleanup finalizer", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
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

		It("selects the exact image for every supported OpenShift minor", func() {
			for _, minor := range []string{"4.18", "4.19", "4.20", "4.21", "4.22"} {
				r, c := newFakeReconciler(minor+".0", map[string]string{
					"4.18": "plugin:4.18", "4.19": "plugin:4.19",
					"4.20": "plugin:4.20",
					"4.21": "plugin:4.21", "4.22": "plugin:4.22",
				})
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
				Expect(err).NotTo(HaveOccurred())

				deployment := &appsv1.Deployment{}
				Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, deployment)).To(Succeed())
				Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("plugin:" + minor))
			}
		})

		It("unregisters the plugin and removes resources for an unsupported OpenShift release", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{
				"4.18": "plugin:4.18", "4.19": "plugin:4.19",
				"4.20": "plugin:4.20", "4.21": "plugin:4.21",
				"4.22": "plugin:4.22",
			})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			updateConsoleOperatorVersion(c, "4.17.8")

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			expectUnregistered(c)
		})

		It("unregisters the plugin and removes resources for a malformed OpenShift release", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			updateConsoleOperatorVersion(c, "v4.20")

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			expectUnregistered(c)
		})

		It("unregisters the plugin and removes resources when the matching image is missing", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			r.PluginImages = map[string]string{"4.19": "plugin:4.19"}

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			expectUnregistered(c)
		})

		It("updates the Deployment image when the OpenShift minor version changes", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{
				"4.20": "plugin:4.20",
				"4.21": "plugin:4.21",
			})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())

			updateConsoleOperatorVersion(c, "4.21.2")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
			deployment := &appsv1.Deployment{}
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("plugin:4.21"))
		})

		It("preserves OLM digest references and accepts prerelease versions with build metadata", func() {
			image := "registry.example/plugin@sha256:0123456789abcdef"
			r, c := newFakeReconciler("4.22.0-0.nightly-2026-10-01+build.1", map[string]string{"4.22": image})
			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			deployment := &appsv1.Deployment{}
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(image))
		})

		It("cleans up when the console operator is absent or its version status is unusable", func() {
			for _, status := range []interface{}{
				nil,
				map[string]interface{}{},
				map[string]interface{}{"versions": "invalid"},
				map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "other", "version": "4.20.0"}}},
				map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "operator", "version": int64(420)}}},
			} {
				r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
				_, err := r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				operator := &unstructured.Unstructured{}
				operator.SetGroupVersionKind(clusterOperatorGVK)
				Expect(c.Get(ctx, types.NamespacedName{Name: consoleClusterOperator}, operator)).To(Succeed())
				if status == nil {
					Expect(c.Delete(ctx, operator)).To(Succeed())
				} else {
					operator.Object["status"] = status
					Expect(c.Update(ctx, operator)).To(Succeed())
				}
				result, err := r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(pluginReconcileInterval))
				expectUnregistered(c)
			}
		})

		It("rejects releases outside both supported boundaries even if images are configured", func() {
			for _, version := range []string{"4.17.0", "4.23.0", "5.18.0", "4.20.0-", "4.020.0", "4.20.0.1"} {
				r, c := newFakeReconciler(version, map[string]string{"4.17": testPluginImage, "4.23": testPluginImage, "5.18": testPluginImage, "4.20": testPluginImage})
				_, err := r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				expectUnregistered(c)
			}
		})

		It("keeps the installed plugin on transient ClusterOperator lookup failures and retries", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			lookupErr := errors.New("temporary API outage")
			r.Client = interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind() == clusterOperatorGVK {
						return lookupErr
					}
					return underlying.Get(ctx, key, obj, opts...)
				},
			})
			_, err = r.Reconcile(ctx, reconcile.Request{})
			Expect(errors.Is(err, lookupErr)).To(BeTrue())
			Expect(c.Get(ctx, types.NamespacedName{Name: pluginDeploymentName, Namespace: testNamespace}, &appsv1.Deployment{})).To(Succeed())
			r.Client = c
			_, err = r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cleans up if the ClusterOperator API is unavailable", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			r.Client = interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind() == clusterOperatorGVK {
						return &apimeta.NoKindMatchError{GroupKind: clusterOperatorGVK.GroupKind()}
					}
					return underlying.Get(ctx, key, obj, opts...)
				},
			})
			_, err = r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			expectUnregistered(c)
		})

		It("returns ConsolePlugin lookup, finalizer update and deletion failures and retries cleanup", func() {
			for _, operation := range []string{"get", "update", "delete"} {
				r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
				_, err := r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				r.PluginImages = nil
				apiErr := errors.New("temporary ConsolePlugin API failure")
				r.Client = interceptor.NewClient(c, interceptor.Funcs{
					Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if operation == "get" && obj.GetObjectKind().GroupVersionKind() == consolePluginGVK {
							return apiErr
						}
						return underlying.Get(ctx, key, obj, opts...)
					},
					Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if operation == "update" && obj.GetObjectKind().GroupVersionKind() == consolePluginGVK {
							return apiErr
						}
						return underlying.Update(ctx, obj, opts...)
					},
					Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if operation == "delete" && obj.GetObjectKind().GroupVersionKind() == consolePluginGVK {
							return apiErr
						}
						return underlying.Delete(ctx, obj, opts...)
					},
				})
				_, err = r.Reconcile(ctx, reconcile.Request{})
				Expect(errors.Is(err, apiErr)).To(BeTrue())
				r.Client = c
				_, err = r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				expectUnregistered(c)
			}
		})

		It("retains the cleanup finalizer on deletion failures and succeeds on retry", func() {
			for _, deleting := range []bool{false, true} {
				r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
				_, err := r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				plugin := &unstructured.Unstructured{}
				plugin.SetGroupVersionKind(consolePluginGVK)
				Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(Succeed())
				if deleting {
					Expect(c.Delete(ctx, plugin)).To(Succeed())
				} else {
					r.PluginImages = nil
				}
				deleteErr := errors.New("temporary delete failure")
				r.Client = interceptor.NewClient(c, interceptor.Funcs{
					Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							return deleteErr
						}
						return underlying.Delete(ctx, obj, opts...)
					},
				})
				_, err = r.Reconcile(ctx, reconcile.Request{})
				Expect(errors.Is(err, deleteErr)).To(BeTrue())
				Expect(c.Get(ctx, types.NamespacedName{Name: consolePluginCRName}, plugin)).To(Succeed())
				Expect(controllerutil.ContainsFinalizer(plugin, pluginCleanupFinalizer)).To(BeTrue())
				r.Client = c
				_, err = r.Reconcile(ctx, reconcile.Request{})
				Expect(err).NotTo(HaveOccurred())
				expectUnregistered(c)
			}
		})

		It("cleans up orphaned resources when the ConsolePlugin is already absent", func() {
			r, c := newFakeReconciler("4.23.0", nil)
			Expect(r.ensureServiceAccount(ctx)).To(Succeed())
			Expect(r.ensureRBAC(ctx)).To(Succeed())
			Expect(r.ensureDeployment(ctx, testPluginImage)).To(Succeed())
			Expect(r.ensureService(ctx)).To(Succeed())
			_, err := r.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())
			expectUnregistered(c)
		})

		It("cleans up namespace-scoped resources and removes the finalizer when the ConsolePlugin CR is deleted", func() {
			r, c := newFakeReconciler("4.20.0", map[string]string{"4.20": testPluginImage})
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
