package controller

import (
	"context"
	"crypto/sha256"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	ocmetrics "github.com/mariusbertram/oc-mirror-operator/pkg/metrics"
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
// secrets: create+update are required because ensureCoordinatorRBAC grants the
// coordinator the right to create/update the worker token secret. Kubernetes
// RBAC prevents privilege escalation: the operator can only grant verbs it
// holds itself. Read-only on foreign secrets would therefore fail. This code
// does NOT mutate any secrets it does not own.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// roles/rolebindings: list+watch are required because controller-runtime builds
// typed-cache informers for CreateOrUpdate (reads go through the cache).
// delete/patch/escalate/bind are intentionally omitted to limit blast radius in
// case of an operator pod compromise. RoleBinding/Role are garbage-collected via
// OwnerReference.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// persistentvolumeclaims: the operator must hold PVC verbs itself so that it
// can grant them to the coordinator Role (Kubernetes RBAC anti-escalation).
// Worker pods use generic ephemeral volumes which require PVC create on behalf
// of the pod creator (the coordinator/manager).
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

func (r *MirrorTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, rerr error) {
	l := log.FromContext(ctx)

	defer func() {
		if rerr != nil {
			ocmetrics.ReconcileErrorsTotal.WithLabelValues(req.Namespace, req.Name, "mirrortarget").Inc()
		}
	}()

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, req.NamespacedName, mt); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !mt.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, mt)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
		controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
		if err := r.Update(ctx, mt); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // update event will re-trigger reconcile
	}

	// Detect removed ImageSets and create cleanup Jobs if cleanup policy is set.
	if err := r.reconcileCleanup(ctx, mt); err != nil {
		l.Error(err, "Failed to reconcile cleanup")
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		// Return error so we retry and don't advance KnownImageSets prematurely.
		return ctrl.Result{}, err
	}

	// Ensure the coordinator ServiceAccount, Role, and RoleBinding exist in the target namespace.
	// The manager deployment runs as coordinator and needs permissions to manage ImageSet status and worker pods.
	if err := r.ensureCoordinatorRBAC(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure coordinator RBAC")
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Default-deny ingress + scoped allow rules so worker pods can only talk to
	// the manager status API and the cluster DNS service. Egress to remote
	// registries is intentionally left open so users can extend with their own
	// stricter policies if needed.
	if err := r.ensureNetworkPolicies(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure network policies")
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Ensure global Resource API Deployment and Service (Phase 7d)
	if err := r.ensureResourceAPI(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure Resource API")
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
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
			"app":                                 "oc-mirror-manager",
			"mirrortarget":                        mt.Name,
			"app.kubernetes.io/component":         "manager",
			"app.kubernetes.io/name":              "oc-mirror",
			"app.kubernetes.io/instance":          mt.Name,
			"oc-mirror.openshift.io/mirrortarget": mt.Name,
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
				Spec: managerPodSpec(mt),
			},
		}
		return nil
	})
	if err != nil {
		l.Error(err, "Failed to reconcile manager deployment")
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Check deployment status
	deploymentReady := false
	if deployment.Status.ReadyReplicas > 0 {
		deploymentReady = true
	}

	if !deploymentReady {
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "DeploymentNotReady", "Waiting for manager deployment to become ready", mt.Generation)
	} else {
		setCondition(&mt.Status.Conditions, conditionTypeReady, metav1.ConditionTrue, "DeploymentReady", "Manager deployment is active", mt.Generation)
	}

	// Only advance KnownImageSets when no pending cleanups remain.
	// This ensures that if a cleanup Job fails, the removal is re-detected
	// and a new cleanup Job is created on the next reconcile cycle.
	if len(mt.Status.PendingCleanup) == 0 {
		mt.Status.KnownImageSets = make([]string, len(mt.Spec.ImageSets))
		copy(mt.Status.KnownImageSets, mt.Spec.ImageSets)
		sort.Strings(mt.Status.KnownImageSets)
	}
	// Aggregate per-ImageSet progress so users see overall rollout state on
	// the MirrorTarget itself (e.g. via `oc get mirrortarget`) without
	// having to query each ImageSet individually.
	if err := r.aggregateImageSetStatus(ctx, mt); err != nil {
		l.Error(err, "Failed to aggregate ImageSet status; continuing with stale counters")
	}
	ocmetrics.MirrorTargetImagesTotal.WithLabelValues(mt.Namespace, mt.Name).Set(float64(mt.Status.TotalImages))
	ocmetrics.MirrorTargetImagesMirrored.WithLabelValues(mt.Namespace, mt.Name).Set(float64(mt.Status.MirroredImages))
	ocmetrics.MirrorTargetImagesFailed.WithLabelValues(mt.Namespace, mt.Name).Set(float64(mt.Status.FailedImages))
	ocmetrics.MirrorTargetImagesPending.WithLabelValues(mt.Namespace, mt.Name).Set(float64(mt.Status.PendingImages))
	if err := r.Status().Update(ctx, mt); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue more frequently while cleanups are in progress to detect completion.
	if len(mt.Status.PendingCleanup) > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// managerPodSpec returns the PodSpec for the manager deployment.
func managerPodSpec(mt *mirrorv1alpha1.MirrorTarget) corev1.PodSpec {
	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "quay.io/mariusbertram/oc-mirror-operator:latest"
	}

	// Pull secret: the operator (and thus the manager) needs credentials for
	// the target registry. Only set imagePullSecrets when one is configured;
	// an explicit empty list would disable the node's default credential chain.
	var imagePullSecrets []corev1.LocalObjectReference
	if mt.Spec.PullSecretRef != nil && mt.Spec.PullSecretRef.Name != "" {
		imagePullSecrets = []corev1.LocalObjectReference{{Name: mt.Spec.PullSecretRef.Name}}
	}

	securityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsNonRoot:             boolPtr(true),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	return corev1.PodSpec{
		ServiceAccountName: mt.Name + "-coordinator",
		ImagePullSecrets:   imagePullSecrets,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   boolPtr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{
			{
				Name:            "manager",
				Image:           operatorImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/manager", "manager", "--target", mt.Name},
				Env: []corev1.EnvVar{
					{Name: "TARGET_NAME", Value: mt.Name},
					{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					{Name: "WORKER_IMAGE", Value: operatorImage},
					{Name: "OPERATOR_IMAGE", Value: operatorImage},
					{Name: "DOCKER_CONFIG", Value: "/var/run/secrets/pull-secret"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pull-secret", MountPath: "/var/run/secrets/pull-secret", ReadOnly: true},
				},
				Ports: []corev1.ContainerPort{
					{Name: "status-api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				SecurityContext: securityContext,
			},
		},
		Volumes: managerVolumes(mt),
	}
}

func managerVolumes(mt *mirrorv1alpha1.MirrorTarget) []corev1.Volume {
	// Always include the pull-secret volume, pointing at the referenced secret
	// (or a placeholder name when none is configured, so the volume spec is
	// valid and the container can start with an empty mount).
	pullSecretName := "oc-mirror-pull-secret-placeholder"
	if mt.Spec.PullSecretRef != nil && mt.Spec.PullSecretRef.Name != "" {
		pullSecretName = mt.Spec.PullSecretRef.Name
	}
	return []corev1.Volume{
		{
			Name: "pull-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: pullSecretName,
					Optional:   boolPtr(true),
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// resourceQuantity is a helper for constructing resource.Quantity values.
func resourceQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// handleDeletion processes MirrorTarget deletion: cleans up
// resources and removes the finalizer.
func (r *MirrorTargetReconciler) handleDeletion(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Perform cleanup: delete any associated Jobs, ConfigMaps, etc.
	l.Info("Handling deletion of MirrorTarget", "name", mt.Name)

	// Remove the finalizer to allow deletion to proceed.
	controllerutil.RemoveFinalizer(mt, mirrorTargetFinalizer)
	if err := r.Update(ctx, mt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileCleanup detects ImageSets that have been removed from
// spec.imageSets and creates cleanup Jobs for them when the cleanup
// policy annotation is set.
func (r *MirrorTargetReconciler) reconcileCleanup(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	l := log.FromContext(ctx)

	// Build the set of currently desired ImageSets.
	desiredSet := make(map[string]struct{}, len(mt.Spec.ImageSets))
	for _, name := range mt.Spec.ImageSets {
		desiredSet[name] = struct{}{}
	}

	// Walk KnownImageSets (written by the previous reconcile cycle)
	// and detect removals.
	var pendingCleanup []string
	for _, known := range mt.Status.KnownImageSets {
		if _, stillDesired := desiredSet[known]; stillDesired {
			continue
		}
		// ImageSet was removed. Check if a cleanup Job is already running.
		cleanupJobName := fmt.Sprintf("%s-cleanup", known)
		cleanupJob := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Namespace: mt.Namespace, Name: cleanupJobName}, cleanupJob)
		switch {
		case err == nil:
			// Job exists — check if it's still running.
			if cleanupJob.Status.Active > 0 || (cleanupJob.Status.Succeeded == 0 && cleanupJob.Status.Failed == 0) {
				pendingCleanup = append(pendingCleanup, known)
			}
			// Succeeded or Failed: job is done, don't add to pending.
		case errors.IsNotFound(err):
			// Check if the cleanup policy annotation is set on the ImageSet.
			// Since the ImageSet may be gone too, we fall back to the MirrorTarget annotation.
			cleanupPolicy := mt.Annotations[mirrorv1alpha1.CleanupPolicyAnnotation]
			if cleanupPolicy == "Delete" {
				l.Info("Creating cleanup Job for removed ImageSet", "imageSet", known)
				if createErr := r.ensureCleanupJob(ctx, mt, known); createErr != nil {
					l.Error(createErr, "Failed to create cleanup Job", "imageSet", known)
				}
				pendingCleanup = append(pendingCleanup, known)
			}
		default:
			return fmt.Errorf("check cleanup job %s: %w", cleanupJobName, err)
		}
	}

	mt.Status.PendingCleanup = pendingCleanup
	return nil
}

// ensureCleanupJob creates a Kubernetes Job that deletes orphaned images
// from the target registry for the removed ImageSet.
func (r *MirrorTargetReconciler) ensureCleanupJob(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSetName string) error {
	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "quay.io/mariusbertram/oc-mirror-operator:latest"
	}

	ttl := int32(600)
	backoffLimit := int32(3)

	securityContext := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsNonRoot:             boolPtr(true),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	pullSecretName := ""
	if mt.Spec.PullSecretRef != nil {
		pullSecretName = mt.Spec.PullSecretRef.Name
	}

	var imagePullSecrets []corev1.LocalObjectReference
	if pullSecretName != "" {
		imagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-cleanup", imageSetName),
			Namespace: mt.Namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ImagePullSecrets:   imagePullSecrets,
					ServiceAccountName: mt.Name + "-coordinator",
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{
						{
							Name:            "cleanup",
							Image:           operatorImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/manager", "cleanup", "--imageset", imageSetName, "--target", mt.Name},
							Env: []corev1.EnvVar{
								{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
								{Name: "DOCKER_CONFIG", Value: "/var/run/secrets/pull-secret"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "pull-secret", MountPath: "/var/run/secrets/pull-secret", ReadOnly: true},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resourceQuantity("100m"),
									corev1.ResourceMemory: resourceQuantity("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resourceQuantity("500m"),
									corev1.ResourceMemory: resourceQuantity("512Mi"),
								},
							},
							SecurityContext: securityContext,
						},
					},
					Volumes: managerVolumes(mt),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(mt, job, r.Scheme); err != nil {
		return err
	}
	return r.Client.Create(ctx, job)
}

// ensureCoordinatorRBAC creates or updates the ServiceAccount, Role and
// RoleBinding that the manager pod uses to manage worker pods and ImageSet
// status.
func (r *MirrorTargetReconciler) ensureCoordinatorRBAC(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: mt.Name + "-coordinator", Namespace: mt.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(mt, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensure coordinator ServiceAccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: mt.Name + "-coordinator", Namespace: mt.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := controllerutil.SetControllerReference(mt, role, r.Scheme); err != nil {
			return err
		}
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"imagesets", "imagesets/status"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"mirrortargets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list", "watch", "create", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure coordinator Role: %w", err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: mt.Name + "-coordinator", Namespace: mt.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		if err := controllerutil.SetControllerReference(mt, rb, r.Scheme); err != nil {
			return err
		}
		rb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      mt.Name + "-coordinator",
			Namespace: mt.Namespace,
		}}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     mt.Name + "-coordinator",
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure coordinator RoleBinding: %w", err)
	}

	return nil
}

// ensureNetworkPolicies creates or updates the default-deny ingress NetworkPolicy
// plus a scoped allow rule for the manager status API.
func (r *MirrorTargetReconciler) ensureNetworkPolicies(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	// 1. Default-deny all ingress for worker pods.
	denyAll := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-worker-deny-all", mt.Name),
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, denyAll, func() error {
		if err := controllerutil.SetControllerReference(mt, denyAll, r.Scheme); err != nil {
			return err
		}
		denyAll.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"oc-mirror.openshift.io/role": "worker"},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure worker deny-all NetworkPolicy: %w", err)
	}

	// 2. Allow worker → manager status API (port 8080).
	port8080 := intstr.FromInt(8080)
	allowManager := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-worker-allow-manager", mt.Name),
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, allowManager, func() error {
		if err := controllerutil.SetControllerReference(mt, allowManager, r.Scheme); err != nil {
			return err
		}
		allowManager.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":          "oc-mirror-manager",
					"mirrortarget": mt.Name,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"oc-mirror.openshift.io/role": "worker"},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &port8080},
					},
				},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure worker-allow-manager NetworkPolicy: %w", err)
	}

	return nil
}

// ensureResourceAPI creates or updates the Deployment and Service for the
// resource API server (port 8000). The resource API is a separate process from
// the manager and serves IDMS/ITMS YAML, catalog info, and target status to
// the ConsolePlugin and CLI.
func (r *MirrorTargetReconciler) ensureResourceAPI(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "quay.io/mariusbertram/oc-mirror-operator:latest"
	}

	svcName := fmt.Sprintf("%s-resources", mt.Name)

	// Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: mt.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(mt, svc, r.Scheme); err != nil {
			return err
		}
		svc.Spec.Selector = map[string]string{
			"app":          "oc-mirror-manager",
			"mirrortarget": mt.Name,
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "resource-api",
			Port:       8000,
			TargetPort: intstr.FromInt(8000),
			Protocol:   corev1.ProtocolTCP,
		}}
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		return nil
	}); err != nil {
		return fmt.Errorf("ensure resource-api Service: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MirrorTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorTarget{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&batchv1.Job{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(
			&mirrorv1alpha1.ImageSet{},
			handler.EnqueueRequestsFromMapFunc(r.imageSetToMirrorTarget),
		).
		Complete(r)
}

func (r *MirrorTargetReconciler) imageSetToMirrorTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	is, ok := obj.(*mirrorv1alpha1.ImageSet)
	if !ok {
		return nil
	}

	mtList := &mirrorv1alpha1.MirrorTargetList{}
	if err := r.List(ctx, mtList, client.InNamespace(is.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, mt := range mtList.Items {
		for _, name := range mt.Spec.ImageSets {
			if name == is.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{
						Name:      mt.Name,
						Namespace: mt.Namespace,
					},
				})
				break
			}
		}
	}
	return requests
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
		return nil
	}
	return nil
}

// hasRouteAPI checks if the OpenShift Route CRD is installed in the cluster.
func (r *MirrorTargetReconciler) hasRouteAPI(ctx context.Context) bool {
	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "RouteList",
	})
	err := r.List(ctx, routes, client.InNamespace("default"), client.Limit(1))
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false
		}
		return false
	}
	return true
}

func (r *MirrorTargetReconciler) ensureRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	route.SetName(svcName)
	route.SetNamespace(mt.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		if err := controllerutil.SetControllerReference(mt, route, r.Scheme); err != nil {
			return err
		}
		spec := map[string]interface{}{
			"to": map[string]interface{}{
				"kind":   "Service",
				"name":   svcName,
				"weight": int64(100),
			},
			"port": map[string]interface{}{
				"targetPort": "resource-api",
			},
			"tls": map[string]interface{}{
				"termination": "edge",
			},
		}
		if err := unstructured.SetNestedField(route.Object, spec, "spec"); err != nil {
			return err
		}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) deleteRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	route.SetName(fmt.Sprintf("%s-resources", mt.Name))
	route.SetNamespace(mt.Namespace)
	_ = r.Client.Delete(ctx, route)
}

func (r *MirrorTargetReconciler) ensureIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: mt.Namespace,
		},
	}
	pathType := networkingv1.PathTypePrefix
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		if err := controllerutil.SetControllerReference(mt, ingress, r.Scheme); err != nil {
			return err
		}
		ingress.Spec = networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: svcName,
											Port: networkingv1.ServiceBackendPort{Number: 8000},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) deleteIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-resources", mt.Name),
			Namespace: mt.Namespace,
		},
	}
	_ = r.Client.Delete(ctx, ingress)
}

// aggregateImageSetStatus walks spec.imageSets and builds per-ImageSet summaries
// for MirrorTarget.Status.ImageSetStatuses. The MirrorTarget-level totals
// (TotalImages, MirroredImages, PendingImages, FailedImages) are derived from
// the consolidated per-MirrorTarget imagestate ConfigMap so that destination
// images shared across multiple ImageSets are counted only once.
// Per-ImageSet summaries still reflect per-ImageSet counts for the breakdown view.
// ImageSets that don't exist (yet) appear with Found=false.
// The per-ImageSet breakdown is sorted alphabetically for deterministic diffs.
func (r *MirrorTargetReconciler) aggregateImageSetStatus(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	names := make([]string, len(mt.Spec.ImageSets))
	copy(names, mt.Spec.ImageSets)
	sort.Strings(names)

	summaries := make([]mirrorv1alpha1.ImageSetStatusSummary, 0, len(names))

	for _, name := range names {
		var is mirrorv1alpha1.ImageSet
		err := r.Get(ctx, client.ObjectKey{Namespace: mt.Namespace, Name: name}, &is)
		if err != nil {
			if errors.IsNotFound(err) {
				summaries = append(summaries, mirrorv1alpha1.ImageSetStatusSummary{
					Name:  name,
					Found: false,
				})
				continue
			}
			return fmt.Errorf("get ImageSet %q: %w", name, err)
		}
		summaries = append(summaries, mirrorv1alpha1.ImageSetStatusSummary{
			Name:     name,
			Found:    true,
			Total:    is.Status.TotalImages,
			Mirrored: is.Status.MirroredImages,
			Pending:  is.Status.PendingImages,
			Failed:   is.Status.FailedImages,
		})
	}

	mt.Status.ImageSetStatuses = summaries

	// Derive MirrorTarget-level totals from the consolidated imagestate so that
	// images shared across multiple ImageSets are counted only once.
	// Fall back to summing per-ImageSet counters when the ConfigMap is absent
	// (empty state, no error) or temporarily unavailable (non-nil error), so
	// status is never silently zeroed before the Manager has written any state.
	state, err := imagestate.LoadForTarget(ctx, r.Client, mt.Namespace, mt.Name)
	if err != nil || len(state) == 0 {
		var total, mirrored, pending, failed int
		for i := range summaries {
			if !summaries[i].Found {
				continue
			}
			total += summaries[i].Total
			mirrored += summaries[i].Mirrored
			pending += summaries[i].Pending
			failed += summaries[i].Failed
		}
		mt.Status.TotalImages = total
		mt.Status.MirroredImages = mirrored
		mt.Status.PendingImages = pending
		mt.Status.FailedImages = failed
		return nil
	}
	total, mirrored, pending, failed := imagestate.Counts(state)
	mt.Status.TotalImages = total
	mt.Status.MirroredImages = mirrored
	mt.Status.PendingImages = pending
	mt.Status.FailedImages = failed
	return nil
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
		return nil
	}
	return nil
}
