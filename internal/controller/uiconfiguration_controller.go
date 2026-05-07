package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const (
	uiConfigurationFinalizer = "mirror.openshift.io/uiconfiguration-cleanup"

	dashboardImageEnv    = "DASHBOARD_IMAGE"
	oauthProxyImageEnv   = "OAUTH_PROXY_IMAGE"
	dashboardName        = "oc-mirror-dashboard"
	pluginName           = "oc-mirror-plugin"
	dashboardSAName      = "oc-mirror-dashboard"
	dashboardClusterRole = "oc-mirror-dashboard"
	oauthProxySecretName = "oc-mirror-dashboard-proxy"

	dashboardPort   = 8080
	oauthProxyPort  = 8443
	pluginPort      = 9443
	resourceAPIName = "oc-mirror-resource-api"
	resourceAPIPort = 8081
)

// UIConfigurationReconciler reconciles a UIConfiguration object and owns all
// dashboard resources (Deployment, Service, Ingress, Route, ConsolePlugin).
type UIConfigurationReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string // operator namespace where dashboard resources are created
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=uiconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=uiconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=uiconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=console.openshift.io,resources=consoleplugins,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch

func (r *UIConfigurationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, rerr error) {
	l := log.FromContext(ctx)

	uic := &mirrorv1alpha1.UIConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, uic); err != nil {
		if errors.IsNotFound(err) {
			l.Info("UIConfiguration resource not found, skipping")
			return reconcile.Result{}, nil
		}
		l.Error(err, "failed to fetch UIConfiguration")
		return reconcile.Result{}, err
	}

	if uic.Status.Conditions == nil {
		uic.Status.Conditions = []metav1.Condition{}
	}

	if !uic.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, uic)
	}

	if !controllerutil.ContainsFinalizer(uic, uiConfigurationFinalizer) {
		controllerutil.AddFinalizer(uic, uiConfigurationFinalizer)
		if err := r.Update(ctx, uic); err != nil {
			l.Error(err, "failed to add finalizer")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// Single-instance validation: only one UIConfiguration may exist cluster-wide.
	if err := r.validateSingleInstance(ctx, uic); err != nil {
		l.Error(err, "single-instance validation failed")
		setCondition(&uic.Status.Conditions, "SingleInstanceViolation", metav1.ConditionTrue, "MultipleInstances", err.Error(), uic.Generation)
		uic.Status.Phase = mirrorv1alpha1.UIConfigurationPhaseFailed
		_ = r.Status().Update(ctx, uic)
		return reconcile.Result{}, nil
	}
	setCondition(&uic.Status.Conditions, "SingleInstanceViolation", metav1.ConditionFalse, "SingleInstance", "Only one UIConfiguration exists", uic.Generation)

	if err := r.validateSpec(uic); err != nil {
		l.Error(err, "spec validation failed")
		setCondition(&uic.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ValidationError", err.Error(), uic.Generation)
		uic.Status.Phase = mirrorv1alpha1.UIConfigurationPhaseFailed
		_ = r.Status().Update(ctx, uic)
		return reconcile.Result{}, nil
	}

	// Dashboard resource management — only active when DASHBOARD_IMAGE is configured
	// and a namespace is known. Skipped silently when either is absent.
	dashImage := os.Getenv(dashboardImageEnv)
	if dashImage != "" && r.Namespace != "" {
		oauthImage := os.Getenv(oauthProxyImageEnv)
		if oauthImage == "" {
			oauthImage = "quay.io/openshift/origin-oauth-proxy:latest"
		}
		if err := r.reconcileDashboardResources(ctx, uic, dashImage, oauthImage); err != nil {
			return reconcile.Result{}, err
		}
	}

	setCondition(&uic.Status.Conditions, conditionTypeReady, metav1.ConditionTrue, "ReconcileSuccess", "UIConfiguration is active", uic.Generation)

	if err := r.updateUIConfigurationStatus(ctx, uic); err != nil {
		l.Error(err, "failed to update UIConfiguration status")
		return reconcile.Result{}, err
	}

	l.Info("successfully reconciled UIConfiguration", "name", uic.Name)
	return reconcile.Result{}, nil
}

func (r *UIConfigurationReconciler) reconcileDashboardResources(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration, dashImage, oauthImage string) error {
	l := log.FromContext(ctx)

	if err := r.ensureServiceAccount(ctx, uic); err != nil {
		return err
	}
	if err := r.ensureClusterRBAC(ctx); err != nil {
		return err
	}
	if err := r.ensureOAuthProxySecret(ctx, uic); err != nil {
		return err
	}

	if uic.Spec.TLS != nil && uic.Spec.TLS.Enabled {
		if uic.Spec.TLS.CertSecretRef == nil {
			msg := "TLS enabled but certSecretRef not provided"
			l.Error(nil, msg)
			r.setUIConfigCondition(ctx, uic, "TLSConfigInvalid", metav1.ConditionTrue, "MissingCertSecret", msg)
			return fmt.Errorf("%s", msg)
		}
		certSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      uic.Spec.TLS.CertSecretRef.Name,
			Namespace: r.Namespace,
		}, certSecret); err != nil {
			msg := fmt.Sprintf("TLS certificate secret not found: %s", uic.Spec.TLS.CertSecretRef.Name)
			l.Error(err, msg)
			r.setUIConfigCondition(ctx, uic, "TLSConfigInvalid", metav1.ConditionTrue, "SecretNotFound", msg)
			return fmt.Errorf("%s", msg)
		}
		l.Info("TLS certificate secret validated", "secretName", uic.Spec.TLS.CertSecretRef.Name)
	}

	if err := r.ensureDashboardDeployment(ctx, dashImage, oauthImage, uic); err != nil {
		return err
	}
	if err := r.ensureDashboardService(ctx, uic); err != nil {
		return err
	}

	switch uic.Spec.ExposureType {
	case mirrorv1alpha1.UIExposureTypeIngress:
		if err := r.generateIngress(ctx, uic); err != nil {
			return err
		}
		r.cleanupOldResourcesForType(ctx, []string{"route", "consoleplugin"})

	case mirrorv1alpha1.UIExposureTypeRoute:
		if err := r.generateRoute(ctx, uic); err != nil {
			return err
		}
		r.cleanupOldResourcesForType(ctx, []string{"ingress", "consoleplugin"})

	case mirrorv1alpha1.UIExposureTypeConsolePlugin:
		if err := r.ensurePluginDeployment(ctx, dashImage, uic); err != nil {
			return err
		}
		if err := r.ensurePluginService(ctx, uic); err != nil {
			return err
		}
		if err := r.generateConsolePlugin(ctx, uic); err != nil {
			return err
		}
		r.cleanupOldResourcesForType(ctx, []string{"route", "ingress"})

	case mirrorv1alpha1.UIExposureTypeService:
		fallthrough
	default:
		r.cleanupOldResourcesForType(ctx, []string{"route", "ingress", "consoleplugin"})
	}

	return r.ensureServiceMonitor(ctx, uic)
}

func (r *UIConfigurationReconciler) ensureServiceAccount(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		if err := controllerutil.SetControllerReference(uic, sa, r.Scheme); err != nil {
			return err
		}
		if sa.Annotations == nil {
			sa.Annotations = map[string]string{}
		}
		sa.Annotations["serviceaccounts.openshift.io/oauth-redirectreference.primary"] = fmt.Sprintf(
			`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"%s"}}`,
			dashboardName,
		)
		return nil
	})
	return err
}

func (r *UIConfigurationReconciler) ensureClusterRBAC(ctx context.Context) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		cr.Rules = []rbacv1.PolicyRule{
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
	})
	if err != nil {
		return err
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     dashboardName,
		}
		crb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      dashboardName,
			Namespace: r.Namespace,
		}}
		return nil
	})
	if err != nil {
		return err
	}

	// oauth-proxy with --provider=openshift requires the SA to be able to validate
	// tokens via TokenReview and SubjectAccessReview — system:auth-delegator grants this.
	authDelegatorCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-auth-delegator"},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, authDelegatorCRB, func() error {
		authDelegatorCRB.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:auth-delegator",
		}
		authDelegatorCRB.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      dashboardName,
			Namespace: r.Namespace,
		}}
		return nil
	})
	return err
}

func (r *UIConfigurationReconciler) ensureOAuthProxySecret(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-proxy", Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if err := controllerutil.SetControllerReference(uic, secret, r.Scheme); err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, exists := secret.Data["session_secret"]; !exists {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			secret.Data["session_secret"] = b
		}
		return nil
	})
	return err
}

func (r *UIConfigurationReconciler) ensureDashboardDeployment(ctx context.Context, dashImage, oauthImage string, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)

	replicas := int32(1)
	if uiConfig.Spec.Replicas != nil && *uiConfig.Spec.Replicas > 0 {
		replicas = *uiConfig.Spec.Replicas
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace},
	}

	l.Info("Ensuring dashboard deployment with pass-access-token and user:full scope")
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if err := controllerutil.SetControllerReference(uiConfig, dep, r.Scheme); err != nil {
			return err
		}
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": dashboardName},
		}

		podSpec := corev1.PodSpec{
			ServiceAccountName: dashboardName,
			SecurityContext:    &corev1.PodSecurityContext{},
			Containers: []corev1.Container{
				{
					Name:  "dashboard",
					Image: dashImage,
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: dashboardPort, Protocol: corev1.ProtocolTCP},
					},
					SecurityContext: restrictedContainerSecurityContext(),
				},
				{
					Name:  "oauth-proxy",
					Image: oauthImage,
					Args: []string{
						"--https-address=:8443",
						"--provider=openshift",
						"--openshift-service-account=" + dashboardName,
						"--upstream=http://localhost:8080",
						"--tls-cert=/etc/tls/private/tls.crt",
						"--tls-key=/etc/tls/private/tls.key",
						"--cookie-secret-file=/etc/proxy/secrets/session_secret",
						"--openshift-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
						"--pass-access-token",
						"--scope=user:full",
					},
					Ports: []corev1.ContainerPort{
						{Name: "https", ContainerPort: oauthProxyPort, Protocol: corev1.ProtocolTCP},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "proxy-tls", MountPath: "/etc/tls/private"},
						{Name: "proxy-secret", MountPath: "/etc/proxy/secrets"},
					},
					SecurityContext: restrictedContainerSecurityContext(),
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "proxy-tls",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: dashboardName + "-tls"},
					},
				},
				{
					Name: "proxy-secret",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: dashboardName + "-proxy"},
					},
				},
			},
		}

		if uiConfig.Spec.Resources != nil {
			for i := range podSpec.Containers {
				podSpec.Containers[i].Resources = *uiConfig.Spec.Resources
			}
			l.Info("applied resource limits to dashboard deployment containers",
				"requests", uiConfig.Spec.Resources.Requests,
				"limits", uiConfig.Spec.Resources.Limits)
		}

		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": dashboardName},
			},
			Spec: podSpec,
		}

		return nil
	})

	if err != nil {
		l.Error(err, "failed to create/update Dashboard Deployment")
	}
	return err
}

func (r *UIConfigurationReconciler) ensureDashboardService(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dashboardName,
			Namespace: r.Namespace,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": dashboardName + "-tls",
			},
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(uic, svc, r.Scheme); err != nil {
			return err
		}
		svc.Spec.Selector = map[string]string{"app": dashboardName}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: oauthProxyPort, TargetPort: intstr.FromInt32(oauthProxyPort), Protocol: corev1.ProtocolTCP},
			{Name: "http", Port: dashboardPort, TargetPort: intstr.FromInt32(dashboardPort), Protocol: corev1.ProtocolTCP},
		}
		return nil
	})
	return err
}

// generateRoute creates or updates an OpenShift Route for the dashboard.
func (r *UIConfigurationReconciler) generateRoute(ctx context.Context, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)

	routeName := dashboardName
	if uiConfig.Spec.RouteName != "" {
		routeName = uiConfig.Spec.RouteName
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(routeName)
	route.SetNamespace(r.Namespace)

	existing := route.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if apimeta.IsNoMatchError(err) {
		l.Info("Route CRD unavailable, skipping Route creation (non-OpenShift cluster)")
		return nil
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		if err := controllerutil.SetControllerReference(uiConfig, route, r.Scheme); err != nil {
			return err
		}
		spec := map[string]interface{}{
			"to": map[string]interface{}{
				"kind":   "Service",
				"name":   dashboardName,
				"weight": int64(100),
			},
			"port": map[string]interface{}{
				"targetPort": "https",
			},
			"tls": map[string]interface{}{
				"termination":                   "reencrypt",
				"insecureEdgeTerminationPolicy": "Redirect",
			},
		}

		if uiConfig.Spec.Hostname != "" {
			spec["host"] = uiConfig.Spec.Hostname
		}

		route.Object["spec"] = spec
		return nil
	})

	if err != nil {
		l.Error(err, "failed to create/update Route")
		return err
	}

	if exposedHost, ok, _ := unstructured.NestedString(route.Object, "status", "ingress", "0", "host"); ok && exposedHost != "" {
		uiConfig.Status.ExposedURL = "https://" + exposedHost
	}

	return nil
}

func (r *UIConfigurationReconciler) ensurePluginDeployment(ctx context.Context, dashImage string, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-plugin", Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if err := controllerutil.SetControllerReference(uiConfig, dep, r.Scheme); err != nil {
			return err
		}
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": dashboardName + "-plugin"},
		}

		pluginContainer := corev1.Container{
			Name:    "plugin",
			Image:   dashImage,
			Command: []string{"/dashboard", "plugin"},
			Args: []string{
				fmt.Sprintf("--bind-address=:%d", pluginPort),
				"--cert-file=/var/serving-cert/tls.crt",
				"--key-file=/var/serving-cert/tls.key",
			},
			Ports: []corev1.ContainerPort{
				{Name: "https", ContainerPort: pluginPort, Protocol: corev1.ProtocolTCP},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "serving-cert", MountPath: "/var/serving-cert"},
			},
			SecurityContext: restrictedContainerSecurityContext(),
		}

		if uiConfig != nil && uiConfig.Spec.Resources != nil {
			pluginContainer.Resources = *uiConfig.Spec.Resources
			l.Info("applied resource limits to plugin deployment container",
				"requests", uiConfig.Spec.Resources.Requests,
				"limits", uiConfig.Spec.Resources.Limits)
		}

		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": dashboardName + "-plugin"}},
			Spec: corev1.PodSpec{
				ServiceAccountName: dashboardName,
				SecurityContext:    &corev1.PodSecurityContext{},
				Containers:         []corev1.Container{pluginContainer},
				Volumes: []corev1.Volume{
					{
						Name: "serving-cert",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: dashboardName + "-plugin-tls"},
						},
					},
				},
			},
		}
		return nil
	})
	return err
}

func (r *UIConfigurationReconciler) ensurePluginService(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dashboardName + "-plugin",
			Namespace: r.Namespace,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": dashboardName + "-plugin-tls",
			},
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(uic, svc, r.Scheme); err != nil {
			return err
		}
		svc.Spec.Selector = map[string]string{"app": dashboardName + "-plugin"}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: pluginPort, TargetPort: intstr.FromInt32(pluginPort), Protocol: corev1.ProtocolTCP},
		}
		return nil
	})
	return err
}

// generateConsolePlugin creates or updates a Console Plugin CR for the dashboard.
func (r *UIConfigurationReconciler) generateConsolePlugin(ctx context.Context, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)
	plugin := &unstructured.Unstructured{}
	plugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsolePlugin",
	})
	plugin.SetName("oc-mirror-operator")

	existing := plugin.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if apimeta.IsNoMatchError(err) {
		l.Info("ConsolePlugin CRD unavailable, skipping ConsolePlugin creation (non-OpenShift cluster)")
		return nil
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, plugin, func() error {
		if err := controllerutil.SetOwnerReference(uiConfig, plugin, r.Scheme); err != nil {
			return err
		}
		plugin.Object["spec"] = map[string]interface{}{
			"displayName": "oc-mirror-operator",
			"backend": map[string]interface{}{
				"type": "Service",
				"service": map[string]interface{}{
					"namespace": r.Namespace,
					"name":      dashboardName + "-plugin",
					"port":      int64(pluginPort),
					"basePath":  "/",
				},
			},
			"proxy": []interface{}{
				map[string]interface{}{
					"alias":         "resourceapi",
					"authorization": "UserToken",
					"endpoint": map[string]interface{}{
						"type": "Service",
						"service": map[string]interface{}{
							"namespace": r.Namespace,
							"name":      dashboardName,
							"port":      int64(dashboardPort),
						},
					},
				},
			},
		}
		return nil
	})

	if err != nil {
		l.Error(err, "failed to create/update ConsolePlugin")
		return err
	}

	// Try to find the console URL to provide a better ExposedURL status.
	consoleRoute := &unstructured.Unstructured{}
	consoleRoute.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: "console", Namespace: "openshift-console"}, consoleRoute); err == nil {
		if host, ok, _ := unstructured.NestedString(consoleRoute.Object, "spec", "host"); ok {
			uiConfig.Status.ExposedURL = "https://" + host + "/oc-mirror/targets"
		} else {
			uiConfig.Status.ExposedURL = "https://<console-host>/oc-mirror/targets"
		}
	} else {
		uiConfig.Status.ExposedURL = "https://<console-host>/oc-mirror/targets"
	}

	return nil
}

// ensureServiceMonitor creates a ServiceMonitor for controller-manager metrics if
// prometheus-operator is installed. Silently skips when the CRD is unavailable.
func (r *UIConfigurationReconciler) ensureServiceMonitor(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	sm.SetName("oc-mirror-manager-metrics")
	sm.SetNamespace(r.Namespace)

	err := r.Get(ctx, client.ObjectKeyFromObject(sm), sm)
	if apimeta.IsNoMatchError(err) {
		l.Info("ServiceMonitor CRD unavailable, skipping (prometheus-operator not installed)")
		return nil
	}
	if errors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(uic, sm, r.Scheme); err != nil {
			return err
		}
		sm.Object["spec"] = map[string]interface{}{
			"endpoints": []interface{}{
				map[string]interface{}{
					"path":   "/metrics",
					"port":   "metrics",
					"scheme": "http",
				},
			},
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/component": "manager",
					"app.kubernetes.io/name":      "oc-mirror",
				},
			},
		}
		return r.Create(ctx, sm)
	}
	return err
}

// generateIngress creates or updates a Kubernetes Ingress for the dashboard.
func (r *UIConfigurationReconciler) generateIngress(ctx context.Context, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)

	hostname := uiConfig.Spec.Hostname
	if hostname == "" {
		hostname = dashboardName + ".example.com"
	}

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dashboardName,
			Namespace: r.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		ingress.Spec.IngressClassName = nil
		if uiConfig.Spec.IngressClassName != "" {
			ingress.Spec.IngressClassName = &uiConfig.Spec.IngressClassName
		}

		ingress.Spec.Rules = []networkingv1.IngressRule{
			{
				Host: hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: func() *networkingv1.PathType { pt := networkingv1.PathTypePrefix; return &pt }(),
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: dashboardName,
										Port: networkingv1.ServiceBackendPort{Number: oauthProxyPort},
									},
								},
							},
						},
					},
				},
			},
		}

		if uiConfig.Spec.TLS != nil && uiConfig.Spec.TLS.Enabled && uiConfig.Spec.TLS.CertSecretRef != nil {
			ingress.Spec.TLS = []networkingv1.IngressTLS{
				{
					Hosts:      []string{hostname},
					SecretName: uiConfig.Spec.TLS.CertSecretRef.Name,
				},
			}
		}

		return nil
	})

	if err != nil {
		l.Error(err, "failed to create/update Ingress")
		return err
	}

	if hostname != "" {
		scheme := "http"
		if uiConfig.Spec.TLS != nil && uiConfig.Spec.TLS.Enabled {
			scheme = "https"
		}
		uiConfig.Status.ExposedURL = fmt.Sprintf("%s://%s", scheme, hostname)
	}

	return nil
}

// cleanupOldResourcesForType deletes resources of specific types when switching exposure types.
func (r *UIConfigurationReconciler) cleanupOldResourcesForType(ctx context.Context, resourceTypes []string) {
	l := log.FromContext(ctx)

	for _, resType := range resourceTypes {
		switch resType {
		case "ingress":
			ingress := &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dashboardName,
					Namespace: r.Namespace,
				},
			}
			if err := r.Delete(ctx, ingress, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
				l.Error(err, "failed to delete old Ingress")
			}

		case "route":
			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "route.openshift.io",
				Version: "v1",
				Kind:    "Route",
			})
			route.SetName(dashboardName)
			route.SetNamespace(r.Namespace)
			if err := r.Delete(ctx, route, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
				l.Error(err, "failed to delete old Route")
			}

		case "consoleplugin":
			plugin := &unstructured.Unstructured{}
			plugin.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "console.openshift.io",
				Version: "v1",
				Kind:    "ConsolePlugin",
			})
			plugin.SetName("oc-mirror-operator")
			if err := r.Delete(ctx, plugin, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
				l.Error(err, "failed to delete old ConsolePlugin")
			}
		}
	}
}

// updateUIConfigurationStatus updates the UIConfiguration status with phase, observed
// generation, TLS condition state, and current deployment replica counts.
func (r *UIConfigurationReconciler) updateUIConfigurationStatus(ctx context.Context, uiConfig *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)

	uiConfig.Status.Phase = mirrorv1alpha1.UIConfigurationPhaseActive
	uiConfig.Status.ObservedGeneration = uiConfig.Generation

	if uiConfig.Spec.TLS == nil || !uiConfig.Spec.TLS.Enabled {
		apimeta.RemoveStatusCondition(&uiConfig.Status.Conditions, "TLSConfigInvalid")
	} else {
		apimeta.SetStatusCondition(&uiConfig.Status.Conditions, metav1.Condition{
			Type:               "TLSConfigValid",
			Status:             metav1.ConditionTrue,
			Reason:             "TLSCertificateSecretValid",
			Message:            "TLS certificate secret validated and mounted",
			ObservedGeneration: uiConfig.Generation,
			LastTransitionTime: metav1.Now(),
		})
		apimeta.RemoveStatusCondition(&uiConfig.Status.Conditions, "TLSConfigInvalid")
	}

	if r.Namespace != "" {
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      dashboardName,
			Namespace: r.Namespace,
		}, dep); err == nil {
			uiConfig.Status.DesiredReplicas = *dep.Spec.Replicas
			uiConfig.Status.AvailableReplicas = dep.Status.ReadyReplicas
		}
	}

	if err := r.Status().Update(ctx, uiConfig); err != nil {
		l.Error(err, "failed to update UIConfiguration status")
		return err
	}

	return nil
}

// setUIConfigCondition updates a condition in the UIConfiguration status and persists
// it immediately for better observability on error paths.
func (r *UIConfigurationReconciler) setUIConfigCondition(ctx context.Context, uiConfig *mirrorv1alpha1.UIConfiguration,
	conditionType string, status metav1.ConditionStatus, reason, message string) {
	l := log.FromContext(ctx)
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: uiConfig.Generation,
		LastTransitionTime: metav1.Now(),
	}
	apimeta.SetStatusCondition(&uiConfig.Status.Conditions, condition)

	if err := r.Status().Update(ctx, uiConfig); err != nil {
		l.Error(err, "failed to update UIConfiguration condition status", "condition", conditionType)
	}
}

// validateSingleInstance ensures only one UIConfiguration exists per namespace.
func (r *UIConfigurationReconciler) validateSingleInstance(ctx context.Context, current *mirrorv1alpha1.UIConfiguration) error {
	l := log.FromContext(ctx)

	uicList := &mirrorv1alpha1.UIConfigurationList{}
	if err := r.List(ctx, uicList, client.InNamespace(current.Namespace)); err != nil {
		return fmt.Errorf("failed to list UIConfigurations: %w", err)
	}

	var otherUICs []*mirrorv1alpha1.UIConfiguration
	for i := range uicList.Items {
		uic := &uicList.Items[i]
		if uic.Name != current.Name || uic.UID != current.UID {
			otherUICs = append(otherUICs, uic)
		}
	}

	if len(otherUICs) > 0 {
		details := fmt.Sprintf("Found %d other UIConfiguration(s): ", len(otherUICs))
		for i, other := range otherUICs {
			if i > 0 {
				details += ", "
			}
			details += other.Name
		}
		l.Info("multiple UIConfigurations detected in namespace", "namespace", current.Namespace, "details", details)
		return fmt.Errorf("multiple UIConfigurations found in namespace %q (only one allowed per namespace): %s", current.Namespace, details)
	}

	return nil
}

// validateSpec validates the UIConfiguration spec. Ingress exposure requires TLS.
func (r *UIConfigurationReconciler) validateSpec(uic *mirrorv1alpha1.UIConfiguration) error {
	if uic.Spec.ExposureType == mirrorv1alpha1.UIExposureTypeIngress {
		if uic.Spec.TLS == nil || !uic.Spec.TLS.Enabled {
			return fmt.Errorf("ingress exposure type requires TLS to be enabled")
		}
	}
	return nil
}

// handleDeletion removes dashboard resources and the finalizer when a UIConfiguration is deleted.
func (r *UIConfigurationReconciler) handleDeletion(ctx context.Context, uic *mirrorv1alpha1.UIConfiguration) (reconcile.Result, error) {
	l := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(uic, uiConfigurationFinalizer) {
		l.Info("cleaning up UIConfiguration", "name", uic.Name)

		if r.Namespace != "" {
			_ = r.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-plugin", Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-plugin", Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-proxy", Namespace: r.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationBackground))

			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
			route.SetName(dashboardName)
			route.SetNamespace(r.Namespace)
			_ = r.Delete(ctx, route, client.PropagationPolicy(metav1.DeletePropagationBackground))

			plugin := &unstructured.Unstructured{}
			plugin.SetGroupVersionKind(schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsolePlugin"})
			plugin.SetName("oc-mirror-operator")
			_ = r.Delete(ctx, plugin, client.PropagationPolicy(metav1.DeletePropagationBackground))

			_ = r.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: dashboardName}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: dashboardName}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			_ = r.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-auth-delegator"}}, client.PropagationPolicy(metav1.DeletePropagationBackground))
		}

		controllerutil.RemoveFinalizer(uic, uiConfigurationFinalizer)
		if err := r.Update(ctx, uic); err != nil {
			l.Error(err, "failed to remove finalizer")
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// restrictedContainerSecurityContext returns the minimal restricted security
// context shared by dashboard and plugin containers.
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

// SetupWithManager registers the UIConfigurationReconciler with the manager.
// Single-instance validation is handled inside Reconcile via validateSingleInstance,
// so no extra cross-object watch is needed here.
func (r *UIConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.UIConfiguration{}).
		Complete(r)
}
