package controller

import (
	"context"
	"crypto/rand"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const (
	dashboardName      = "oc-mirror-dashboard"
	dashboardPort      = 8080
	oauthProxyPort     = 8443
	pluginPort         = 9001
	dashboardImageEnv  = "DASHBOARD_IMAGE"
	oauthProxyImageEnv = "OAUTH_PROXY_IMAGE"
)

// DashboardReconciler manages the singleton cluster-wide dashboard Deployment,
// the oauth-proxy sidecar, the Console Plugin backend, and the associated RBAC.
// It is triggered by any MirrorTarget event and bootstrapped at operator startup.
type DashboardReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=console.openshift.io,resources=consoleplugins,verbs=get;list;watch;create;update;patch;delete
func (r *DashboardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info("reconciling dashboard singleton")

	dashImage := os.Getenv(dashboardImageEnv)
	if dashImage == "" {
		l.Info("DASHBOARD_IMAGE not set, skipping dashboard reconciliation")
		return ctrl.Result{}, nil
	}
	oauthImage := os.Getenv(oauthProxyImageEnv)
	if oauthImage == "" {
		oauthImage = "quay.io/openshift/origin-oauth-proxy:4.15"
	}

	if err := r.ensureServiceAccount(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureClusterRBAC(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureOAuthProxySecret(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDashboardDeployment(ctx, dashImage, oauthImage); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDashboardService(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRoute(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePluginDeployment(ctx, dashImage); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePluginService(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureConsolePlugin(ctx); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *DashboardReconciler) ensureServiceAccount(ctx context.Context) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error { return nil })
	return err
}

func (r *DashboardReconciler) ensureClusterRBAC(ctx context.Context) error {
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
	return err
}

func (r *DashboardReconciler) ensureOAuthProxySecret(ctx context.Context) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-proxy", Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
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

func (r *DashboardReconciler) ensureDashboardDeployment(ctx context.Context, dashImage, oauthImage string) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName, Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": dashboardName},
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": dashboardName},
				Annotations: map[string]string{
					// oauth-proxy reads this annotation to discover the cookie secret.
					"serviceaccounts.openshift.io/oauth-redirectreference.primary": `{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"` + dashboardName + `"}}`,
				},
			},
			Spec: corev1.PodSpec{
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
			},
		}
		return nil
	})
	return err
}

func (r *DashboardReconciler) ensureDashboardService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dashboardName,
			Namespace: r.Namespace,
			Annotations: map[string]string{
				// OpenShift service-CA auto-generates the TLS certificate for the Service.
				"service.beta.openshift.io/serving-cert-secret-name": dashboardName + "-tls",
			},
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = map[string]string{"app": dashboardName}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: oauthProxyPort, TargetPort: intstr.FromInt32(oauthProxyPort), Protocol: corev1.ProtocolTCP},
		}
		return nil
	})
	return err
}

func (r *DashboardReconciler) ensureRoute(ctx context.Context) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(dashboardName)
	route.SetNamespace(r.Namespace)

	existing := route.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if errors.IsNotFound(err) {
		route.Object["spec"] = map[string]interface{}{
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
		return r.Create(ctx, route)
	}
	return err
}

func (r *DashboardReconciler) ensurePluginDeployment(ctx context.Context, dashImage string) error {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-plugin", Namespace: r.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": dashboardName + "-plugin"},
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": dashboardName + "-plugin"}},
			Spec: corev1.PodSpec{
				ServiceAccountName: dashboardName,
				SecurityContext:    &corev1.PodSecurityContext{},
				Containers: []corev1.Container{
					{
						Name:    "plugin",
						Image:   dashImage,
						Command: []string{"/dashboard", "plugin"},
						Args: []string{
							"--bind-address=:9001",
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
					},
				},
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

func (r *DashboardReconciler) ensurePluginService(ctx context.Context) error {
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
		svc.Spec.Selector = map[string]string{"app": dashboardName + "-plugin"}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: pluginPort, TargetPort: intstr.FromInt32(pluginPort), Protocol: corev1.ProtocolTCP},
		}
		return nil
	})
	return err
}

func (r *DashboardReconciler) ensureConsolePlugin(ctx context.Context) error {
	plugin := &unstructured.Unstructured{}
	plugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "console.openshift.io",
		Version: "v1",
		Kind:    "ConsolePlugin",
	})
	plugin.SetName("oc-mirror-operator")

	existing := plugin.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if errors.IsNotFound(err) {
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
		}
		return r.Client.Create(ctx, plugin)
	}
	return err
}

// SetupWithManager registers the DashboardReconciler and adds a startup runnable
// that triggers a reconcile before the first MirrorTarget event arrives.
func (r *DashboardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Bootstrap: reconcile once as soon as the cache is synced.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		_, err := r.Reconcile(ctx, ctrl.Request{})
		return err
	})); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorTarget{}).
		Complete(r)
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
