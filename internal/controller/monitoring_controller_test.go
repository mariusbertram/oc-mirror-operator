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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
		It("should create the dashboard ConfigMap and return RequeueAfter when ServiceMonitor CRD is unavailable", func() {
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testNS}}
			result, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// ServiceMonitor CRD is not registered in the fake scheme, so the
			// reconciler logs a skip message and returns RequeueAfter.
			Expect(result.RequeueAfter).To(Equal(monitoringReconcileInterval))

			// Dashboard ConfigMap must have been created even on the CRD-skip path.
			cm := &corev1.ConfigMap{}
			Expect(r.Client.Get(ctx, types.NamespacedName{
				Name:      dashboardConfigMapName,
				Namespace: dashboardConfigMapNamespace,
			}, cm)).To(Succeed())
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
