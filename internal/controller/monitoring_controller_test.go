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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Monitoring Controller", func() {
	const testNS = "oc-mirror-system"

	var (
		ctx context.Context
		r   *MonitoringReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		r = &MonitoringReconciler{
			Client:    fakeClient,
			Scheme:    scheme,
			Namespace: testNS,
		}
	})

	Context("ensureDashboardConfigMap", func() {
		It("should create the dashboard ConfigMap with the correct label", func() {
			Expect(r.ensureDashboardConfigMap(ctx)).To(Succeed())

			cm := &corev1.ConfigMap{}
			Expect(r.Client.Get(ctx, types.NamespacedName{
				Name:      dashboardConfigMapName,
				Namespace: dashboardConfigMapNamespace,
			}, cm)).To(Succeed())

			Expect(cm.Labels).To(HaveKeyWithValue("console.openshift.io/dashboard", "true"))
			Expect(cm.Data).To(HaveKey("oc-mirror-dashboard.json"))
		})

		It("should be idempotent on repeated calls", func() {
			Expect(r.ensureDashboardConfigMap(ctx)).To(Succeed())
			Expect(r.ensureDashboardConfigMap(ctx)).To(Succeed())

			cm := &corev1.ConfigMap{}
			Expect(r.Client.Get(ctx, types.NamespacedName{
				Name:      dashboardConfigMapName,
				Namespace: dashboardConfigMapNamespace,
			}, cm)).To(Succeed())
			Expect(cm.Labels).To(HaveKey("app.kubernetes.io/managed-by"))
		})
	})

	Context("ensureControllerServiceMonitor", func() {
		It("should create the ServiceMonitor for the controller without error", func() {
			Expect(r.ensureControllerServiceMonitor(ctx)).To(Succeed())
		})

		It("should be idempotent", func() {
			Expect(r.ensureControllerServiceMonitor(ctx)).To(Succeed())
			Expect(r.ensureControllerServiceMonitor(ctx)).To(Succeed())
		})
	})

	Context("ensureManagerServiceMonitor", func() {
		It("should create the ServiceMonitor for the manager without error", func() {
			Expect(r.ensureManagerServiceMonitor(ctx)).To(Succeed())
		})

		It("should be idempotent", func() {
			Expect(r.ensureManagerServiceMonitor(ctx)).To(Succeed())
			Expect(r.ensureManagerServiceMonitor(ctx)).To(Succeed())
		})
	})

	Context("ensurePrometheusRule", func() {
		It("should create the PrometheusRule without error", func() {
			Expect(r.ensurePrometheusRule(ctx)).To(Succeed())
		})

		It("should be idempotent", func() {
			Expect(r.ensurePrometheusRule(ctx)).To(Succeed())
			Expect(r.ensurePrometheusRule(ctx)).To(Succeed())
		})
	})

	Context("Reconcile", func() {
		It("skips the dashboard without error on plain Kubernetes (no OpenShift console API)", func() {
			// The fake client's default RESTMapper knows neither the
			// ConsolePlugin nor the ServiceMonitor GVK — a plain Kubernetes
			// cluster without prometheus-operator (#188).
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testNS}}
			result, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(monitoringReconcileInterval))

			cm := &corev1.ConfigMap{}
			err = r.Get(ctx, types.NamespacedName{
				Name:      dashboardConfigMapName,
				Namespace: dashboardConfigMapNamespace,
			}, cm)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no dashboard ConfigMap outside OpenShift, got %v", err)
		})

		It("creates the dashboard ConfigMap on OpenShift even when the ServiceMonitor CRD is unavailable", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{consolePluginGVK.GroupVersion()})
			rm.Add(consolePluginGVK, apimeta.RESTScopeRoot)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(rm).Build()
			fr := &MonitoringReconciler{Client: fakeClient, Scheme: scheme, Namespace: testNS}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testNS}}
			result, err := fr.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(monitoringReconcileInterval))

			cm := &corev1.ConfigMap{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{
				Name:      dashboardConfigMapName,
				Namespace: dashboardConfigMapNamespace,
			}, cm)).To(Succeed())
		})
	})

	// envtest's real API server (used elsewhere in this package) has no
	// prometheus-operator CRDs installed, and the fake client's default
	// RESTMapper (used by the Context above) knows nothing about ServiceMonitor
	// either — so Reconcile's CRD-gate always takes the "unavailable" branch in
	// both. This uses a fake client seeded with a RESTMapper that knows about
	// the ServiceMonitor GVK to exercise the actual happy path.
	Context("Reconcile with the ServiceMonitor CRD available (fake RESTMapper)", func() {
		It("creates both ServiceMonitors and the PrometheusRule", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)

			rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{serviceMonitorGVK.GroupVersion()})
			rm.Add(serviceMonitorGVK, apimeta.RESTScopeNamespace)
			rm.Add(prometheusRuleGVK, apimeta.RESTScopeNamespace)

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(rm).Build()
			fr := &MonitoringReconciler{Client: fakeClient, Scheme: scheme, Namespace: testNS}

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testNS}}
			result, err := fr.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(monitoringReconcileInterval))

			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: controllerServiceMonitorName, Namespace: testNS}, sm)).To(Succeed())

			sm2 := &unstructured.Unstructured{}
			sm2.SetGroupVersionKind(serviceMonitorGVK)
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: managerServiceMonitorName, Namespace: testNS}, sm2)).To(Succeed())

			pr := &unstructured.Unstructured{}
			pr.SetGroupVersionKind(prometheusRuleGVK)
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: prometheusRuleName, Namespace: testNS}, pr)).To(Succeed())

			// Second reconcile is idempotent.
			_, err = fr.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("enqueueForDashboardConfigMap", func() {
		It("returns a reconcile request for the matching ConfigMap", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dashboardConfigMapName,
					Namespace: dashboardConfigMapNamespace,
				},
			}
			requests := r.enqueueForDashboardConfigMap(ctx, cm)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal(testNS))
		})

		It("returns nil for a different ConfigMap name", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "some-other-cm",
					Namespace: dashboardConfigMapNamespace,
				},
			}
			Expect(r.enqueueForDashboardConfigMap(ctx, cm)).To(BeNil())
		})

		It("returns nil for a different namespace", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dashboardConfigMapName,
					Namespace: "wrong-namespace",
				},
			}
			Expect(r.enqueueForDashboardConfigMap(ctx, cm)).To(BeNil())
		})
	})
})
