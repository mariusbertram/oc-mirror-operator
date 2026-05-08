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
	_ "embed"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

//go:embed assets/oc-mirror-dashboard.json
var dashboardJSON []byte

const (
	monitoringReconcileInterval = 10 * time.Minute

	controllerServiceMonitorName = "oc-mirror-controller"
	managerServiceMonitorName    = "oc-mirror-manager"
	prometheusRuleName           = "oc-mirror-alerts"
	dashboardConfigMapName       = "oc-mirror-dashboard"
)

var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
}

var prometheusRuleGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "PrometheusRule",
}

// MonitoringReconciler is a singleton controller that automatically manages
// ServiceMonitor, PrometheusRule, and the Grafana dashboard ConfigMap whenever
// the prometheus-operator CRDs are available on the cluster. It requires no
// user-facing CRD.
type MonitoringReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete

func (r *MonitoringReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := log.FromContext(ctx)

	// The dashboard ConfigMap is a plain corev1.ConfigMap — always reconcile it.
	if err := r.ensureDashboardConfigMap(ctx); err != nil {
		return reconcile.Result{}, err
	}

	// ServiceMonitor and PrometheusRule require prometheus-operator CRDs.
	if _, err := r.RESTMapper().RESTMapping(serviceMonitorGVK.GroupKind(), serviceMonitorGVK.Version); err != nil {
		l.Info("ServiceMonitor CRD unavailable, skipping monitoring resources (prometheus-operator not installed)")
		return reconcile.Result{RequeueAfter: monitoringReconcileInterval}, nil
	}

	if err := r.ensureControllerServiceMonitor(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureManagerServiceMonitor(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensurePrometheusRule(ctx); err != nil {
		return reconcile.Result{}, err
	}

	l.Info("successfully reconciled monitoring resources")
	return reconcile.Result{RequeueAfter: monitoringReconcileInterval}, nil
}

// ensureDashboardConfigMap creates or updates the Grafana dashboard ConfigMap.
// The label console.openshift.io/dashboard: "true" causes the OpenShift web
// console monitoring plugin to expose it under Observe > Dashboards.
func (r *MonitoringReconciler) ensureDashboardConfigMap(ctx context.Context) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dashboardConfigMapName,
			Namespace: r.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels["app.kubernetes.io/name"] = "oc-mirror"
		cm.Labels["app.kubernetes.io/managed-by"] = "oc-mirror-operator"
		cm.Labels["console.openshift.io/dashboard"] = "true"
		cm.Data = map[string]string{
			"oc-mirror-dashboard.json": string(dashboardJSON),
		}
		return nil
	})
	return err
}

// ensureControllerServiceMonitor creates or updates the ServiceMonitor for the
// controller-manager metrics endpoint (HTTPS, port 8443).
func (r *MonitoringReconciler) ensureControllerServiceMonitor(ctx context.Context) error {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(controllerServiceMonitorName)
	sm.SetNamespace(r.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		labels := map[string]interface{}{
			"app.kubernetes.io/name":       "oc-mirror",
			"app.kubernetes.io/managed-by": "oc-mirror-operator",
		}
		if err := unstructured.SetNestedMap(sm.Object, labels, "metadata", "labels"); err != nil {
			return err
		}
		endpoints := []interface{}{
			map[string]interface{}{
				"path":            "/metrics",
				"port":            "https",
				"scheme":          "https",
				"bearerTokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token",
				"tlsConfig": map[string]interface{}{
					"insecureSkipVerify": true,
				},
			},
		}
		if err := unstructured.SetNestedSlice(sm.Object, endpoints, "spec", "endpoints"); err != nil {
			return err
		}
		selector := map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"control-plane":          "controller-manager",
				"app.kubernetes.io/name": "oc-mirror",
			},
		}
		return unstructured.SetNestedMap(sm.Object, selector, "spec", "selector")
	})
	return err
}

// ensureManagerServiceMonitor creates or updates the ServiceMonitor for the
// manager pod metrics endpoints (HTTP, port 9090). One Service per MirrorTarget
// is created by the MirrorTargetReconciler, all with the same labels.
func (r *MonitoringReconciler) ensureManagerServiceMonitor(ctx context.Context) error {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(managerServiceMonitorName)
	sm.SetNamespace(r.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		labels := map[string]interface{}{
			"app.kubernetes.io/name":       "oc-mirror",
			"app.kubernetes.io/managed-by": "oc-mirror-operator",
		}
		if err := unstructured.SetNestedMap(sm.Object, labels, "metadata", "labels"); err != nil {
			return err
		}
		endpoints := []interface{}{
			map[string]interface{}{
				"path":   "/metrics",
				"port":   "metrics",
				"scheme": "http",
			},
		}
		if err := unstructured.SetNestedSlice(sm.Object, endpoints, "spec", "endpoints"); err != nil {
			return err
		}
		selector := map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"app.kubernetes.io/component": "manager",
				"app.kubernetes.io/name":      "oc-mirror",
			},
		}
		return unstructured.SetNestedMap(sm.Object, selector, "spec", "selector")
	})
	return err
}

// ensurePrometheusRule creates or updates the PrometheusRule with OC Mirror alerts.
func (r *MonitoringReconciler) ensurePrometheusRule(ctx context.Context) error {
	pr := &unstructured.Unstructured{}
	pr.SetGroupVersionKind(prometheusRuleGVK)
	pr.SetName(prometheusRuleName)
	pr.SetNamespace(r.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pr, func() error {
		labels := map[string]interface{}{
			"app.kubernetes.io/name":       "oc-mirror",
			"app.kubernetes.io/managed-by": "oc-mirror-operator",
		}
		if err := unstructured.SetNestedMap(pr.Object, labels, "metadata", "labels"); err != nil {
			return err
		}
		groups := []interface{}{
			map[string]interface{}{
				"name":     "oc-mirror.rules",
				"interval": "30s",
				"rules": []interface{}{
					map[string]interface{}{
						"alert": "OCMirrorHighFailedImages",
						"expr":  "oc_mirror_mirrortarget_images_failed > 10",
						"for":   "5m",
						"labels": map[string]interface{}{
							"severity": "warning",
						},
						"annotations": map[string]interface{}{
							"summary":     "High number of failed images for {{ $labels.target }}",
							"description": "MirrorTarget {{ $labels.namespace }}/{{ $labels.target }} has {{ $value }} permanently failed images for more than 5 minutes.",
						},
					},
					map[string]interface{}{
						"alert": "OCMirrorAllImagesFailed",
						"expr":  "oc_mirror_mirrortarget_images_failed / on(namespace, target) oc_mirror_mirrortarget_images_total > 0.5",
						"for":   "10m",
						"labels": map[string]interface{}{
							"severity": "critical",
						},
						"annotations": map[string]interface{}{
							"summary":     "More than 50% of images failed for {{ $labels.target }}",
							"description": "MirrorTarget {{ $labels.namespace }}/{{ $labels.target }} has more than 50% permanently failed images for more than 10 minutes.",
						},
					},
					map[string]interface{}{
						"alert": "OCMirrorReconcileErrors",
						"expr":  "rate(oc_mirror_reconcile_errors_total[5m]) > 0",
						"for":   "0m",
						"labels": map[string]interface{}{
							"severity": "warning",
						},
						"annotations": map[string]interface{}{
							"summary":     "Reconcile errors in controller for {{ $labels.name }}",
							"description": "The oc-mirror {{ $labels.controller }} controller is encountering reconcile errors for {{ $labels.namespace }}/{{ $labels.name }}.",
						},
					},
					map[string]interface{}{
						"alert": "OCMirrorNoProgress",
						"expr":  "oc_mirror_mirrortarget_images_pending > 0 and rate(oc_mirror_manager_images_mirrored_total[30m]) == 0",
						"for":   "30m",
						"labels": map[string]interface{}{
							"severity": "warning",
						},
						"annotations": map[string]interface{}{
							"summary":     "No mirroring progress for {{ $labels.target }}",
							"description": "MirrorTarget {{ $labels.target }} has pending images but no images have been mirrored in the last 30 minutes.",
						},
					},
					map[string]interface{}{
						"alert": "OCMirrorManagerDown",
						"expr":  "absent(oc_mirror_manager_active_workers)",
						"for":   "5m",
						"labels": map[string]interface{}{
							"severity": "critical",
						},
						"annotations": map[string]interface{}{
							"summary":     "OC Mirror manager metrics absent",
							"description": "No manager metrics are being reported. The manager pod may be down.",
						},
					},
				},
			},
		}
		return unstructured.SetNestedSlice(pr.Object, groups, "spec", "groups")
	})
	return err
}

// SetupWithManager registers the MonitoringReconciler. The operator Namespace
// is used as the initial trigger — identical to ConsolePluginReconciler.
func (r *MonitoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring").
		For(&corev1.Namespace{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == r.Namespace
		}))).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueForDashboardConfigMap),
		).
		Complete(r)
}

// enqueueForDashboardConfigMap re-enqueues the singleton reconcile request
// when the dashboard ConfigMap is changed or deleted.
func (r *MonitoringReconciler) enqueueForDashboardConfigMap(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != dashboardConfigMapName || obj.GetNamespace() != r.Namespace {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: r.Namespace}},
	}
}
