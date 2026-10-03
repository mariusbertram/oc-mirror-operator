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
	"fmt"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	consolePluginCRName     = "oc-mirror-operator"
	consoleClusterOperator  = "console"
	pluginSAName            = "oc-mirror-plugin"
	pluginDeploymentName    = "oc-mirror-plugin"
	pluginServiceName       = "oc-mirror-plugin"
	pluginRoleName          = "oc-mirror-plugin"
	pluginPort              = int32(9443)
	pluginTLSSecretName     = "oc-mirror-plugin-tls"
	pluginReconcileInterval = 10 * time.Minute

	// pluginCleanupFinalizer is added to the ConsolePlugin CR so that all
	// namespace-scoped plugin resources (Deployment, Service, SA, RBAC) are
	// deleted before the CR is fully removed.
	pluginCleanupFinalizer = "mirror.openshift.io/plugin-cleanup"

	// legacyDashboardServiceName is the name used by the old UIConfiguration-based
	// dashboard deployment. The consoleplugin reconciler deletes these stale resources
	// on first reconcile so they don't interfere with the new plugin setup.
	legacyDashboardServiceName = "oc-mirror-dashboard"
)

var supportedConsolePluginMinors = map[string]struct{}{
	"4.18": {},
	"4.19": {},
	"4.20": {},
	"4.21": {},
	"4.22": {},
}

// ConsolePluginReconciler is a singleton controller that automatically manages
// the ConsolePlugin and its supporting resources whenever the ConsolePlugin CRD
// is available (OpenShift clusters). It requires no user-facing CRD.
type ConsolePluginReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Namespace    string // operator namespace
	PluginImages map[string]string
}

// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=console.openshift.io,resources=consoleplugins,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=get;list;watch

func (r *ConsolePluginReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := log.FromContext(ctx)

	consolePluginGVK := schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsolePlugin",
	}
	if _, err := r.RESTMapper().RESTMapping(consolePluginGVK.GroupKind(), consolePluginGVK.Version); err != nil {
		if !apimeta.IsNoMatchError(err) {
			return reconcile.Result{}, fmt.Errorf("discover ConsolePlugin API: %w", err)
		}
		l.Info("ConsolePlugin CRD unavailable, skipping (non-OpenShift cluster)")
		return reconcile.Result{RequeueAfter: pluginReconcileInterval}, nil
	}

	// Check if the ConsolePlugin CR is being deleted. If so, clean up all
	// namespace-scoped plugin resources and remove our finalizer so the CR
	// can be fully garbage-collected.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(consolePluginGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: consolePluginCRName}, existing); err == nil {
		if !existing.GetDeletionTimestamp().IsZero() {
			l.Info("ConsolePlugin CR is being deleted, cleaning up plugin resources")
			if err := r.deletePluginResources(ctx); err != nil {
				return reconcile.Result{}, fmt.Errorf("delete plugin resources while finalizing ConsolePlugin: %w", err)
			}
			controllerutil.RemoveFinalizer(existing, pluginCleanupFinalizer)
			if err := r.Update(ctx, existing); err != nil {
				return reconcile.Result{}, fmt.Errorf("remove plugin-cleanup finalizer: %w", err)
			}
			return reconcile.Result{}, nil
		}
	} else if client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("get ConsolePlugin resource: %w", err)
	}

	release, reason, err := r.consoleReleaseMinor(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	if reason != "" {
		l.Error(errors.New(reason), "cannot select ConsolePlugin image; unregistering plugin")
		if err := r.unregisterConsolePlugin(ctx, consolePluginGVK); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: pluginReconcileInterval}, nil
	}
	if _, supported := supportedConsolePluginMinors[release]; !supported {
		l.Info("OpenShift release is not supported by this ConsolePlugin; unregistering plugin", "release", release)
		if err := r.unregisterConsolePlugin(ctx, consolePluginGVK); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: pluginReconcileInterval}, nil
	}
	pluginImage := r.PluginImages[release]
	if pluginImage == "" {
		l.Error(fmt.Errorf("environment variable %s is empty", pluginImageEnvironmentVariable(release)),
			"ConsolePlugin image is not configured for supported OpenShift release; unregistering plugin", "release", release)
		if err := r.unregisterConsolePlugin(ctx, consolePluginGVK); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: pluginReconcileInterval}, nil
	}

	if err := r.ensureServiceAccount(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureRBAC(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureDeployment(ctx, pluginImage); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureService(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.ensureConsolePlugin(ctx); err != nil {
		return reconcile.Result{}, err
	}
	r.cleanupLegacyDashboard(ctx)

	l.Info("successfully reconciled ConsolePlugin resources")
	return reconcile.Result{RequeueAfter: pluginReconcileInterval}, nil
}

func (r *ConsolePluginReconciler) consoleReleaseMinor(ctx context.Context) (string, string, error) {
	clusterOperatorGVK := schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "ClusterOperator",
	}
	operator := &unstructured.Unstructured{}
	operator.SetGroupVersionKind(clusterOperatorGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: consoleClusterOperator}, operator); err != nil {
		if apimeta.IsNoMatchError(err) {
			return "", "ClusterOperator API is unavailable", nil
		}
		if client.IgnoreNotFound(err) == nil {
			return "", "ClusterOperator console was not found", nil
		}
		return "", "", fmt.Errorf("get ClusterOperator console: %w", err)
	}

	versions, found, err := unstructured.NestedSlice(operator.Object, "status", "versions")
	if err != nil {
		return "", fmt.Sprintf("ClusterOperator console has malformed status.versions: %v", err), nil
	}
	if !found {
		return "", "ClusterOperator console status.versions is missing", nil
	}

	for _, entry := range versions {
		versionEntry, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(versionEntry, "name")
		if name != "operator" {
			continue
		}
		version, _, _ := unstructured.NestedString(versionEntry, "version")
		parsed, err := semver.Parse(version)
		if err != nil {
			return "", fmt.Sprintf("ClusterOperator console operator version %q is malformed", version), nil
		}
		return fmt.Sprintf("%d.%d", parsed.Major, parsed.Minor), "", nil
	}
	return "", "ClusterOperator console status.versions has no operator version", nil
}

func pluginImageEnvironmentVariable(minor string) string {
	return "RELATED_IMAGE_PLUGIN_" + strings.ReplaceAll(minor, ".", "_")
}

// cleanupLegacyDashboard removes stale Service and Route resources from the old
// UIConfiguration-based dashboard deployment. These have no ownerReferences so
// Kubernetes garbage collection will not clean them up automatically. Errors are
// logged but not returned — missing resources are not an error.
func (r *ConsolePluginReconciler) cleanupLegacyDashboard(ctx context.Context) {
	l := log.FromContext(ctx)

	// Delete legacy dashboard Service.
	svc := &corev1.Service{}
	svc.Name = legacyDashboardServiceName
	svc.Namespace = r.Namespace
	if err := r.Delete(ctx, svc); err != nil {
		if client.IgnoreNotFound(err) != nil {
			l.Info("could not delete legacy dashboard service", "err", err)
		}
	} else {
		l.Info("deleted legacy dashboard service", "name", legacyDashboardServiceName)
	}

	// Delete legacy dashboard Route (OpenShift-specific; ignore if Route API unavailable).
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(legacyDashboardServiceName)
	route.SetNamespace(r.Namespace)
	if err := r.Delete(ctx, route); err != nil {
		if client.IgnoreNotFound(err) != nil && !apimeta.IsNoMatchError(err) {
			l.Info("could not delete legacy dashboard route", "err", err)
		}
	} else {
		l.Info("deleted legacy dashboard route", "name", legacyDashboardServiceName)
	}
}

func (r *ConsolePluginReconciler) ensureServiceAccount(ctx context.Context) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pluginSAName,
			Namespace: r.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return nil
	})
	return err
}

func (r *ConsolePluginReconciler) ensureRBAC(ctx context.Context) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pluginRoleName,
			Namespace: r.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"mirrortargets", "imagesets"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update Role: %w", err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pluginRoleName,
			Namespace: r.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     pluginRoleName,
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      pluginSAName,
				Namespace: r.Namespace,
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update RoleBinding: %w", err)
	}

	return nil
}

func (r *ConsolePluginReconciler) ensureDeployment(ctx context.Context, pluginImage string) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pluginDeploymentName,
			Namespace: r.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": pluginDeploymentName},
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": pluginDeploymentName},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: pluginSAName,
				SecurityContext:    &corev1.PodSecurityContext{},
				Containers: []corev1.Container{
					{
						Name:    "plugin",
						Image:   pluginImage,
						Command: []string{"/plugin"},
						Args: []string{
							fmt.Sprintf("--bind-address=:%d", pluginPort),
							"--cert-file=/var/serving-cert/tls.crt",
							"--key-file=/var/serving-cert/tls.key",
						},
						Ports: []corev1.ContainerPort{
							{Name: "https", ContainerPort: pluginPort, Protocol: corev1.ProtocolTCP},
						},
						Env: []corev1.EnvVar{
							{Name: "POD_NAMESPACE", Value: r.Namespace},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("50Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("100Mi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/plugin-manifest.json",
									Port:   intstr.FromInt32(pluginPort),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
							TimeoutSeconds:      3,
							FailureThreshold:    3,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/plugin-manifest.json",
									Port:   intstr.FromInt32(pluginPort),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       30,
							TimeoutSeconds:      5,
							FailureThreshold:    3,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "serving-cert", MountPath: "/var/serving-cert"},
						},
						SecurityContext: restrictedContainerSecurityContext(),
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "serving-cert",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: pluginTLSSecretName},
						},
					},
				},
			},
		}
		return nil
	})
	return err
}

func (r *ConsolePluginReconciler) unregisterConsolePlugin(ctx context.Context, pluginGVK schema.GroupVersionKind) error {
	l := log.FromContext(ctx)
	plugin := &unstructured.Unstructured{}
	plugin.SetGroupVersionKind(pluginGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: consolePluginCRName}, plugin); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("get ConsolePlugin resource for cleanup: %w", err)
		}
		if err := r.deletePluginResources(ctx); err != nil {
			return fmt.Errorf("delete supporting resources for unregistered ConsolePlugin: %w", err)
		}
		return nil
	}

	if err := r.deletePluginResources(ctx); err != nil {
		return fmt.Errorf("delete supporting resources for unsupported ConsolePlugin: %w", err)
	}
	if controllerutil.ContainsFinalizer(plugin, pluginCleanupFinalizer) {
		controllerutil.RemoveFinalizer(plugin, pluginCleanupFinalizer)
		if err := r.Update(ctx, plugin); err != nil {
			return fmt.Errorf("remove plugin-cleanup finalizer while unregistering ConsolePlugin: %w", err)
		}
	}
	if !plugin.GetDeletionTimestamp().IsZero() {
		return nil
	}
	if err := r.Delete(ctx, plugin); err != nil && client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete unsupported ConsolePlugin resource: %w", err)
	}
	l.Info("unregistered ConsolePlugin and removed its supporting resources")
	return nil
}

func (r *ConsolePluginReconciler) ensureService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pluginServiceName,
			Namespace: r.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"] = pluginTLSSecretName
		svc.Spec.Selector = map[string]string{"app": pluginServiceName}
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "https",
				Port:       pluginPort,
				TargetPort: intstr.FromInt32(pluginPort),
				Protocol:   corev1.ProtocolTCP,
			},
		}
		return nil
	})
	return err
}

// ensureConsolePlugin creates or updates the cluster-scoped ConsolePlugin CR
// and ensures our cleanup finalizer is present so that plugin namespace resources
// are removed if the CR is ever deleted.
// Individual spec fields are set via SetNestedField to avoid overwriting
// admission-defaulted fields such as spec.i18n.
func (r *ConsolePluginReconciler) ensureConsolePlugin(ctx context.Context) error {
	l := log.FromContext(ctx)

	plugin := &unstructured.Unstructured{}
	plugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsolePlugin",
	})
	plugin.SetName(consolePluginCRName)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, plugin, func() error {
		// Ensure our cleanup finalizer is present so that the namespace-scoped
		// plugin resources are deleted when the ConsolePlugin CR is removed.
		controllerutil.AddFinalizer(plugin, pluginCleanupFinalizer)

		if err := unstructured.SetNestedField(plugin.Object, "OC Mirror Operator", "spec", "displayName"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(plugin.Object, "Service", "spec", "backend", "type"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(plugin.Object, r.Namespace, "spec", "backend", "service", "namespace"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(plugin.Object, pluginServiceName, "spec", "backend", "service", "name"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(plugin.Object, int64(pluginPort), "spec", "backend", "service", "port"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(plugin.Object, "/", "spec", "backend", "service", "basePath"); err != nil {
			return err
		}
		proxies := []interface{}{
			map[string]interface{}{
				"alias":         "resourceapi",
				"authorization": "UserToken",
				"endpoint": map[string]interface{}{
					"type": "Service",
					"service": map[string]interface{}{
						"namespace": r.Namespace,
						"name":      pluginServiceName,
						"port":      int64(pluginPort),
					},
				},
			},
		}
		if err := unstructured.SetNestedSlice(plugin.Object, proxies, "spec", "proxy"); err != nil {
			return err
		}
		return unstructured.SetNestedField(plugin.Object, "Preload", "spec", "i18n", "loadType")
	})
	if err != nil {
		if apimeta.IsNoMatchError(err) {
			l.Info("ConsolePlugin CRD unavailable, skipping")
			return nil
		}
		l.Error(err, "failed to create/update ConsolePlugin")
		return err
	}
	return nil
}

// deletePluginResources removes all namespace-scoped resources that were created
// by the ConsolePlugin reconciler. Errors are returned so the ConsolePlugin
// cleanup finalizer remains until all resources have been removed.
func (r *ConsolePluginReconciler) deletePluginResources(ctx context.Context) error {
	l := log.FromContext(ctx)
	var errs []error

	dep := &appsv1.Deployment{}
	dep.Name = pluginDeploymentName
	dep.Namespace = r.Namespace
	if err := r.Delete(ctx, dep); client.IgnoreNotFound(err) != nil {
		l.Error(err, "failed to delete plugin Deployment")
		errs = append(errs, fmt.Errorf("delete plugin Deployment: %w", err))
	}

	svc := &corev1.Service{}
	svc.Name = pluginServiceName
	svc.Namespace = r.Namespace
	if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
		l.Error(err, "failed to delete plugin Service")
		errs = append(errs, fmt.Errorf("delete plugin Service: %w", err))
	}

	sa := &corev1.ServiceAccount{}
	sa.Name = pluginSAName
	sa.Namespace = r.Namespace
	if err := r.Delete(ctx, sa); client.IgnoreNotFound(err) != nil {
		l.Error(err, "failed to delete plugin ServiceAccount")
		errs = append(errs, fmt.Errorf("delete plugin ServiceAccount: %w", err))
	}

	rb := &rbacv1.RoleBinding{}
	rb.Name = pluginRoleName
	rb.Namespace = r.Namespace
	if err := r.Delete(ctx, rb); client.IgnoreNotFound(err) != nil {
		l.Error(err, "failed to delete plugin RoleBinding")
		errs = append(errs, fmt.Errorf("delete plugin RoleBinding: %w", err))
	}

	role := &rbacv1.Role{}
	role.Name = pluginRoleName
	role.Namespace = r.Namespace
	if err := r.Delete(ctx, role); client.IgnoreNotFound(err) != nil {
		l.Error(err, "failed to delete plugin Role")
		errs = append(errs, fmt.Errorf("delete plugin Role: %w", err))
	}
	return errors.Join(errs...)
}

// SetupWithManager registers the ConsolePluginReconciler.
//
// The operator Namespace object is used as the initial trigger: when the informer
// cache syncs at startup, an AddEvent fires for the Namespace and kicks off the
// first reconcile without requiring any user-facing CRD.
func (r *ConsolePluginReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bld := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == r.Namespace
		}))).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueForPluginDeployment),
		)

	consolePluginGVK := schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsolePlugin",
	}
	if _, err := mgr.GetRESTMapper().RESTMapping(consolePluginGVK.GroupKind(), consolePluginGVK.Version); err == nil {
		consolePluginObj := &unstructured.Unstructured{}
		consolePluginObj.SetGroupVersionKind(consolePluginGVK)
		bld = bld.Watches(consolePluginObj, handler.EnqueueRequestsFromMapFunc(r.enqueueForConsolePlugin))
	} else if !apimeta.IsNoMatchError(err) {
		return fmt.Errorf("discover ConsolePlugin API for watch: %w", err)
	}
	clusterOperatorGVK := schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "ClusterOperator",
	}
	if _, err := mgr.GetRESTMapper().RESTMapping(clusterOperatorGVK.GroupKind(), clusterOperatorGVK.Version); err == nil {
		clusterOperatorObj := &unstructured.Unstructured{}
		clusterOperatorObj.SetGroupVersionKind(clusterOperatorGVK)
		bld = bld.Watches(clusterOperatorObj, handler.EnqueueRequestsFromMapFunc(r.enqueueForConsoleClusterOperator))
	} else if !apimeta.IsNoMatchError(err) {
		return fmt.Errorf("discover ClusterOperator API for watch: %w", err)
	}

	return bld.Complete(r)
}

// enqueueForPluginDeployment maps a Deployment event to a reconcile request for
// the singleton ConsolePlugin reconciler. Only events for the plugin Deployment
// in the operator namespace trigger reconciliation.
func (r *ConsolePluginReconciler) enqueueForPluginDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != pluginDeploymentName || obj.GetNamespace() != r.Namespace {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: r.Namespace}},
	}
}

// enqueueForConsolePlugin maps a ConsolePlugin event to a reconcile request for
// the singleton. Only events for the well-known plugin name trigger reconciliation.
func (r *ConsolePluginReconciler) enqueueForConsolePlugin(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != consolePluginCRName {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: r.Namespace}},
	}
}

func (r *ConsolePluginReconciler) enqueueForConsoleClusterOperator(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != consoleClusterOperator {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: r.Namespace}},
	}
}

// restrictedContainerSecurityContext returns the minimal restricted security
// context shared by plugin containers.
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	allowPrivilegeEscalation := false
	readOnlyRoot := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   &readOnlyRoot,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
