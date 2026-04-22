package controller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

const mirrorTargetFinalizer = "mirror.openshift.io/cleanup"

// MirrorTargetReconciler reconciles a MirrorTarget object
type MirrorTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *MirrorTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, req.NamespacedName, mt); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !mt.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
			podList := &corev1.PodList{}
			if err := r.List(ctx, podList, client.InNamespace(mt.Namespace),
				client.MatchingLabels{"mirrortarget": mt.Name}); err != nil {
				return ctrl.Result{}, err
			}
			for _, pod := range podList.Items {
				if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
					l.Error(err, "Failed to delete worker pod", "pod", pod.Name)
				}
			}
			controllerutil.RemoveFinalizer(mt, mirrorTargetFinalizer)
			if err := r.Update(ctx, mt); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
		controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
		if err := r.Update(ctx, mt); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Detect removed ImageSets and create cleanup Jobs if cleanup policy is set.
	if err := r.reconcileCleanup(ctx, mt); err != nil {
		l.Error(err, "Failed to reconcile cleanup")
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupError", err.Error())
		_ = r.Status().Update(ctx, mt)
		// Continue with the rest of reconciliation — cleanup failures are not blocking.
	}

	// Ensure the coordinator ServiceAccount, Role, and RoleBinding exist in the target namespace.
	// The manager deployment runs as coordinator and needs permissions to manage ImageSet status and worker pods.
	if err := r.ensureCoordinatorRBAC(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure coordinator RBAC")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Define the manager deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-manager", mt.Name),
			Namespace: mt.Namespace,
		},
	}

	// Use controllerutil.CreateOrUpdate to manage the deployment
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		// Set owner reference
		if err := controllerutil.SetControllerReference(mt, deployment, r.Scheme); err != nil {
			return err
		}

		labels := map[string]string{
			"app":          "oc-mirror-manager",
			"mirrortarget": mt.Name,
		}
		deployment.Labels = labels

		replicas := int32(1)
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "oc-mirror-coordinator",
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: pointerTo(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "manager",
							Image: os.Getenv("OPERATOR_IMAGE"), // Ensure this is set or use a default
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: pointerTo(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Args: []string{
								"manager",
								"--mirrortarget", mt.Name,
								"--namespace", mt.Namespace,
							},
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "OPERATOR_IMAGE",
									Value: os.Getenv("OPERATOR_IMAGE"),
								},
								{
									Name:  "DOCKER_CONFIG",
									Value: "/docker-config",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dockerconfig",
									MountPath: "/docker-config",
									ReadOnly:  true,
								},
							},
							Resources: mt.Spec.Manager.Resources,
							Ports: []corev1.ContainerPort{
								{Name: "status", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
								{Name: "resources", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dockerconfig",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: mt.Spec.AuthSecret,
									Items: []corev1.KeyToPath{
										{Key: "config.json", Path: "config.json"},
									},
								},
							},
						},
					},
					NodeSelector: mt.Spec.Manager.NodeSelector,
					Tolerations:  mt.Spec.Manager.Tolerations,
				},
			},
		}
		return nil
	})
	if err != nil {
		l.Error(err, "Failed to create or update manager deployment")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Define the manager service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-manager", mt.Name),
			Namespace: mt.Namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if err := controllerutil.SetControllerReference(mt, service, r.Scheme); err != nil {
			return err
		}
		service.Labels = map[string]string{
			"app":          "oc-mirror-manager",
			"mirrortarget": mt.Name,
		}
		service.Spec = corev1.ServiceSpec{
			Selector: service.Labels,
			Ports: []corev1.ServicePort{
				{
					Name: "http",
					Port: 8080,
				},
			},
		}
		return nil
	})
	if err != nil {
		l.Error(err, "Failed to create or update manager service")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Resource API Service (port 8081) — serves IDMS, ITMS, CatalogSource, etc.
	resourceSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-resources", mt.Name),
			Namespace: mt.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, resourceSvc, func() error {
		if err := controllerutil.SetControllerReference(mt, resourceSvc, r.Scheme); err != nil {
			return err
		}
		resourceSvc.Labels = map[string]string{
			"app":          "oc-mirror-manager",
			"mirrortarget": mt.Name,
		}
		resourceSvc.Spec = corev1.ServiceSpec{
			Selector: resourceSvc.Labels,
			Ports: []corev1.ServicePort{
				{
					Name: "resources",
					Port: 8081,
				},
			},
		}
		return nil
	})
	if err != nil {
		l.Error(err, "Failed to create or update resources service")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Create Route/Ingress for the resource server based on ExposeConfig.
	if err := r.reconcileExposure(ctx, mt); err != nil {
		l.Error(err, "Failed to reconcile resource server exposure")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionTrue, "DeploymentReady", "Manager deployment is active")
	// Only advance KnownImageSets when no pending cleanups remain.
	// This ensures that if a cleanup Job fails, the removal is re-detected
	// and a new cleanup Job is created on the next reconcile cycle.
	if len(mt.Status.PendingCleanup) == 0 {
		mt.Status.KnownImageSets = make([]string, len(mt.Spec.ImageSets))
		copy(mt.Status.KnownImageSets, mt.Spec.ImageSets)
		sort.Strings(mt.Status.KnownImageSets)
	}
	if err := r.Status().Update(ctx, mt); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue periodically while cleanups are in progress to detect completion.
	if len(mt.Status.PendingCleanup) > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// ensureCoordinatorRBAC creates the ServiceAccount, Role, and RoleBinding needed by the manager pod.
func (r *MirrorTargetReconciler) ensureCoordinatorRBAC(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	// ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oc-mirror-coordinator",
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(mt, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create coordinator ServiceAccount: %w", err)
	}

	// Worker ServiceAccount (used by mirror worker pods)
	workerSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oc-mirror-worker",
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, workerSA, func() error {
		return controllerutil.SetControllerReference(mt, workerSA, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create worker ServiceAccount: %w", err)
	}

	// Role granting coordinator access to manage ImageSets, pods, and MirrorTargets in the namespace
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oc-mirror-coordinator",
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := controllerutil.SetControllerReference(mt, role, r.Scheme); err != nil {
			return err
		}
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"imagesets", "mirrortargets"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"imagesets/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			// Required to set blockOwnerDeletion on worker pods whose owner is a MirrorTarget.
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"mirrortargets/finalizers"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
			// Required to read the authSecret referenced in MirrorTarget.
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			// Required to store and read per-image mirror state.
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create coordinator Role: %w", err)
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oc-mirror-coordinator",
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		if err := controllerutil.SetControllerReference(mt, rb, r.Scheme); err != nil {
			return err
		}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "oc-mirror-coordinator",
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "oc-mirror-coordinator",
				Namespace: mt.Namespace,
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create coordinator RoleBinding: %w", err)
	}

	return nil
}

// reconcileCleanup detects ImageSets that were removed from spec.imageSets,
// creates cleanup Jobs for them (if cleanup-policy annotation is "Delete"),
// and tracks cleanup progress.
func (r *MirrorTargetReconciler) reconcileCleanup(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	l := log.FromContext(ctx)

	// Determine which ImageSets were removed since last reconcile.
	currentSet := make(map[string]bool, len(mt.Spec.ImageSets))
	for _, name := range mt.Spec.ImageSets {
		currentSet[name] = true
	}

	var removed []string
	for _, name := range mt.Status.KnownImageSets {
		if !currentSet[name] {
			removed = append(removed, name)
		}
	}

	// Check pending cleanups — remove entries whose Jobs completed successfully.
	var stillPending []string
	for _, name := range mt.Status.PendingCleanup {
		jobName := cleanupJobName(mt.Name, name)
		job := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: mt.Namespace}, job)
		if errors.IsNotFound(err) {
			// Job is gone. Check if images ConfigMap still exists to decide
			// whether cleanup actually completed or the job was deleted prematurely.
			state, loadErr := imagestate.Load(ctx, r.Client, mt.Namespace, name)
			if loadErr == nil && len(state) > 0 {
				l.Info("Cleanup job gone but image state remains — will re-create job", "imageset", name)
				stillPending = append(stillPending, name)
			} else {
				l.Info("Cleanup job gone and no image state — considering done", "imageset", name)
			}
			continue
		}
		if err != nil {
			stillPending = append(stillPending, name)
			continue
		}
		if job.Status.Succeeded > 0 {
			l.Info("Cleanup completed successfully", "imageset", name, "job", jobName)
			continue
		}
		if job.Status.Failed > 0 {
			l.Error(nil, "Cleanup job failed — will retry", "imageset", name, "job", jobName)
			// Delete the failed job so it can be re-created.
			propagation := metav1.DeletePropagationBackground
			_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation})
		}
		stillPending = append(stillPending, name)
	}
	mt.Status.PendingCleanup = stillPending

	if len(removed) == 0 {
		// No new removals — update condition if pending cleanups just finished.
		if len(mt.Status.PendingCleanup) == 0 {
			for _, c := range mt.Status.Conditions {
				if c.Type == "Cleanup" && c.Reason == "CleanupInProgress" {
					setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionTrue, "CleanupComplete", "No pending cleanups")
					break
				}
			}
		}
		return nil
	}

	// Check if cleanup-policy annotation is set.
	cleanupPolicy := mt.Annotations[mirrorv1alpha1.CleanupPolicyAnnotation]
	if cleanupPolicy != mirrorv1alpha1.CleanupPolicyDelete {
		l.Info("ImageSets removed but cleanup-policy not set to Delete — skipping registry cleanup",
			"removed", removed, "annotation", cleanupPolicy)
		return nil
	}

	// Create cleanup Jobs for each removed ImageSet.
	for _, isName := range removed {
		// Skip if already pending cleanup.
		alreadyPending := false
		for _, p := range mt.Status.PendingCleanup {
			if p == isName {
				alreadyPending = true
				break
			}
		}
		if alreadyPending {
			continue
		}

		// Verify the image state ConfigMap exists (otherwise nothing to clean).
		state, err := imagestate.Load(ctx, r.Client, mt.Namespace, isName)
		if err != nil {
			l.Error(err, "Failed to load image state for cleanup", "imageset", isName)
			continue
		}
		if len(state) == 0 {
			l.Info("No image state found for removed ImageSet — nothing to clean", "imageset", isName)
			continue
		}

		l.Info("Creating cleanup job for removed ImageSet", "imageset", isName, "images", len(state))
		if err := r.createCleanupJob(ctx, mt, isName); err != nil {
			l.Error(err, "Failed to create cleanup job", "imageset", isName)
			continue
		}
		mt.Status.PendingCleanup = append(mt.Status.PendingCleanup, isName)
	}

	if len(mt.Status.PendingCleanup) > 0 {
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupInProgress",
			fmt.Sprintf("Cleaning up images for: %s", strings.Join(mt.Status.PendingCleanup, ", ")))
	} else {
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionTrue, "CleanupComplete", "No pending cleanups")
	}

	return nil
}

// createCleanupJob creates a Kubernetes Job that deletes all images for the
// given ImageSet from the target registry and removes the state ConfigMap.
func (r *MirrorTargetReconciler) createCleanupJob(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSetName string) error {
	jobName := cleanupJobName(mt.Name, imageSetName)

	// Check if job already exists.
	existing := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: mt.Namespace}, existing); err == nil {
		return nil // already exists
	}

	backoffLimit := int32(3)
	ttlAfterFinished := int32(600) // 10 min

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "oc-mirror-operator",
		"mirror.openshift.io/cleanup":  imageSetName,
		"mirrortarget":                 mt.Name,
	}

	args := []string{
		"cleanup",
		"--imageset", imageSetName,
		"--namespace", mt.Namespace,
		"--registry", mt.Spec.Registry,
	}
	if mt.Spec.Insecure {
		args = append(args, "--insecure")
	}

	env := []corev1.EnvVar{}
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if mt.Spec.AuthSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mt.Spec.AuthSecret,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "registry-auth",
			MountPath: "/docker-config",
			ReadOnly:  true,
		})
		env = append(env, corev1.EnvVar{
			Name:  "DOCKER_CONFIG",
			Value: "/docker-config",
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: mt.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mt, mirrorv1alpha1.GroupVersion.WithKind("MirrorTarget")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "oc-mirror-coordinator",
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: pointerTo(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "cleanup",
							Image: os.Getenv("OPERATOR_IMAGE"),
							Args:  args,
							Env:   env,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: pointerTo(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes:      volumes,
					NodeSelector: mt.Spec.Worker.NodeSelector,
					Tolerations:  mt.Spec.Worker.Tolerations,
				},
			},
		},
	}

	return r.Create(ctx, job)
}

// cleanupJobName returns a deterministic, DNS-safe name for cleanup Jobs.
func cleanupJobName(targetName, imageSetName string) string {
	name := "cleanup-" + targetName + "-" + imageSetName
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// SetupWithManager sets up the controller with the Manager.
func (r *MirrorTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorTarget{}).
		Complete(r)
}

// reconcileExposure creates/updates/cleans up Route, Ingress, or HTTPRoute
// based on the MirrorTarget's ExposeConfig.
func (r *MirrorTargetReconciler) reconcileExposure(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	l := log.FromContext(ctx)

	exposeType := mirrorv1alpha1.ExposeTypeService // default
	if mt.Spec.Expose != nil && mt.Spec.Expose.Type != "" {
		exposeType = mt.Spec.Expose.Type
	} else {
		// Auto-detect OpenShift: check if Route API exists.
		if r.hasRouteAPI(ctx) {
			exposeType = mirrorv1alpha1.ExposeTypeRoute
		}
	}

	resourceSvcName := fmt.Sprintf("%s-resources", mt.Name)

	// Clean up exposure objects that don't match desired type.
	if exposeType != mirrorv1alpha1.ExposeTypeRoute {
		r.deleteRoute(ctx, mt)
	}
	if exposeType != mirrorv1alpha1.ExposeTypeIngress {
		r.deleteIngress(ctx, mt)
	}

	switch exposeType {
	case mirrorv1alpha1.ExposeTypeRoute:
		return r.ensureRoute(ctx, mt, resourceSvcName)
	case mirrorv1alpha1.ExposeTypeIngress:
		return r.ensureIngress(ctx, mt, resourceSvcName)
	case mirrorv1alpha1.ExposeTypeService:
		l.Info("Resource server exposed via Service only", "service", resourceSvcName)
		return nil
	case mirrorv1alpha1.ExposeTypeGatewayAPI:
		l.Info("GatewayAPI exposure not yet implemented — using Service only")
		return nil
	default:
		return nil
	}
}

// hasRouteAPI checks if the OpenShift Route API is available.
func (r *MirrorTargetReconciler) hasRouteAPI(ctx context.Context) bool {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName("__probe__")
	route.SetNamespace("default")
	// Try to Get a non-existent Route. If the API is not installed, we get a NoMatch error.
	err := r.Get(ctx, client.ObjectKeyFromObject(route), route)
	if err == nil {
		return true
	}
	// If the error is "no match" the API doesn't exist.
	if meta.IsNoMatchError(err) {
		return false
	}
	// NotFound means the API exists but the object doesn't.
	return errors.IsNotFound(err)
}

// ensureRoute creates or updates an OpenShift Route for the resource server.
func (r *MirrorTargetReconciler) ensureRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	l := log.FromContext(ctx)

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(fmt.Sprintf("%s-resources", mt.Name))
	route.SetNamespace(mt.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		if err := controllerutil.SetControllerReference(mt, route, r.Scheme); err != nil {
			return err
		}
		route.SetLabels(map[string]string{
			"app":          "oc-mirror-resources",
			"mirrortarget": mt.Name,
		})

		spec := map[string]interface{}{
			"to": map[string]interface{}{
				"kind": "Service",
				"name": svcName,
			},
			"port": map[string]interface{}{
				"targetPort": "resources",
			},
			"tls": map[string]interface{}{
				"termination":                   "edge",
				"insecureEdgeTerminationPolicy": "Redirect",
			},
		}
		// Only set host when user explicitly provides it.
		if mt.Spec.Expose != nil && mt.Spec.Expose.Host != "" {
			spec["host"] = mt.Spec.Expose.Host
		}
		route.Object["spec"] = spec
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create/update Route: %w", err)
	}

	l.Info("Route for resource server reconciled", "route", route.GetName())
	return nil
}

// ensureIngress creates or updates a networking.k8s.io/v1 Ingress.
func (r *MirrorTargetReconciler) ensureIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	l := log.FromContext(ctx)

	host := ""
	ingressClass := ""
	if mt.Spec.Expose != nil {
		host = mt.Spec.Expose.Host
		ingressClass = mt.Spec.Expose.IngressClassName
	}
	if host == "" {
		return fmt.Errorf("Ingress exposure requires a host to be set in spec.expose.host")
	}

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-resources", mt.Name),
			Namespace: mt.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		if err := controllerutil.SetControllerReference(mt, ingress, r.Scheme); err != nil {
			return err
		}
		ingress.Labels = map[string]string{
			"app":          "oc-mirror-resources",
			"mirrortarget": mt.Name,
		}
		pathType := networkingv1.PathTypePrefix
		ingress.Spec = networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/resources",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: svcName,
											Port: networkingv1.ServiceBackendPort{Number: 8081},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		if ingressClass != "" {
			ingress.Spec.IngressClassName = &ingressClass
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create/update Ingress: %w", err)
	}

	l.Info("Ingress for resource server reconciled", "ingress", ingress.Name)
	return nil
}

// deleteRoute removes the Route if it exists.
func (r *MirrorTargetReconciler) deleteRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(fmt.Sprintf("%s-resources", mt.Name))
	route.SetNamespace(mt.Namespace)
	_ = r.Delete(ctx, route)
}

// deleteIngress removes the Ingress if it exists.
func (r *MirrorTargetReconciler) deleteIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-resources", mt.Name),
			Namespace: mt.Namespace,
		},
	}
	_ = r.Delete(ctx, ingress)
}

func pointerTo[T any](v T) *T {
	return &v
}
