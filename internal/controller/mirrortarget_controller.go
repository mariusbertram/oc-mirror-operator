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
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// persistentvolumeclaims: the operator must hold PVC verbs itself so that it
// can grant them to the coordinator Role (Kubernetes RBAC anti-escalation).
// Worker pods use generic ephemeral volumes which require PVC create on behalf
// of the pod creator (the coordinator/manager).
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

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
		return r.handleDeletion(ctx, mt)
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
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		// Continue with the rest of reconciliation — cleanup failures are not blocking.
	}

	// Ensure the coordinator ServiceAccount, Role, and RoleBinding exist in the target namespace.
	// The manager deployment runs as coordinator and needs permissions to manage ImageSet status and worker pods.
	if err := r.ensureCoordinatorRBAC(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure coordinator RBAC")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Default-deny ingress + scoped allow rules so worker pods can only talk to
	// the manager status API and the cluster DNS service. Egress to remote
	// registries is intentionally left open so users can extend with their own
	// stricter policies if needed.
	if err := r.ensureNetworkPolicies(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure network policies")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Ensure global Resource API Deployment and Service (Phase 7d)
	if err := r.ensureResourceAPI(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure Resource API")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
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
					ServiceAccountName: mt.Name + "-coordinator",
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
							Env:          managerContainerEnv(mt),
							VolumeMounts: managerContainerVolumeMounts(mt),
							Resources:    mt.Spec.Manager.Resources,
							Ports: []corev1.ContainerPort{
								{Name: "status", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
							},
						},
					},
					Volumes:      managerPodVolumes(mt),
					NodeSelector: mt.Spec.Manager.NodeSelector,
					Tolerations:  mt.Spec.Manager.Tolerations,
				},
			},
		}
		return nil
	})
	if err != nil {
		l.Error(err, "Failed to create or update manager deployment")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
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
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Resource API Service (port 8081) — serves IDMS, ITMS, CatalogSource, etc.
	// Point to the global oc-mirror-resource-api Deployment (Phase 7e)
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
			"app":          "oc-mirror-resource-api",
			"mirrortarget": mt.Name,
		}
		resourceSvc.Spec = corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "oc-mirror-resource-api",
			},
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
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}

	// Create Route/Ingress for the resource server based on ExposeConfig.
	if err := r.reconcileExposure(ctx, mt); err != nil {
		l.Error(err, "Failed to reconcile resource server exposure; continuing with status aggregation")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ExposureError", err.Error(), mt.Generation)
		// We don't return here so that image counts are still aggregated and surfaced to the UI.
	} else {
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionTrue, "DeploymentReady", "Manager deployment is active", mt.Generation)
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
// All resources are named after the MirrorTarget so that multiple MirrorTargets can coexist in the
// same namespace without ownership conflicts.
func (r *MirrorTargetReconciler) ensureCoordinatorRBAC(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	coordinatorName := mt.Name + "-coordinator"
	workerName := mt.Name + "-worker"

	// ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      coordinatorName,
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
			Name:      workerName,
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
			Name:      coordinatorName,
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
				Resources: []string{"imagesets"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				// Manager only reads its own MirrorTarget. Status writes
				// happen through the controller running in the operator
				// namespace, not from inside the manager pod.
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"mirrortargets"},
				Verbs:     []string{"get", "list", "watch"},
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
			// Required to read the authSecret referenced in MirrorTarget and to
			// create/manage the worker bearer-token secret (<target>-worker-token).
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list", "watch", "create", "update"},
			},
			// Required to store and read per-image mirror state.
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			// Required for generic ephemeral volumes on worker pods: the admission
			// controller creates a PVC on behalf of the pod creator (coordinator).
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "create", "delete"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create coordinator Role: %w", err)
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      coordinatorName,
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
			Name:     coordinatorName,
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      coordinatorName,
				Namespace: mt.Namespace,
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create coordinator RoleBinding: %w", err)
	}

	return nil
}

// ensureNetworkPolicies creates a default-deny NetworkPolicy plus narrowly
// scoped allow rules for the manager and worker pods belonging to mt. The
// policies are namespace-scoped and owned by the MirrorTarget so they are
// garbage-collected automatically when the MirrorTarget is deleted.
func (r *MirrorTargetReconciler) ensureNetworkPolicies(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	owner := []metav1.OwnerReference{
		*metav1.NewControllerRef(mt, mirrorv1alpha1.GroupVersion.WithKind("MirrorTarget")),
	}

	managerSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app":          "oc-mirror-manager",
			"mirrortarget": mt.Name,
		},
	}
	workerSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app":          "oc-mirror-worker",
			"mirrortarget": mt.Name,
		},
	}

	tcp := corev1.ProtocolTCP
	statusPort := intstr.FromInt(8080)
	resourcesPort := intstr.FromInt(8081)

	policies := []*networkingv1.NetworkPolicy{
		// 1. Manager ingress policy. Two rules:
		//    a) Status endpoint (8080): only worker pods of the same
		//       MirrorTarget may report status.
		//    b) Resource API (8081): open to all sources so that users,
		//       Ingress controllers, and Routes can reach it.
		// Egress is left unrestricted because the manager talks to the
		// kube-apiserver and remote registries.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            mt.Name + "-manager-ingress",
				Namespace:       mt.Namespace,
				OwnerReferences: owner,
				Labels:          map[string]string{"app.kubernetes.io/managed-by": "oc-mirror-operator"},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: managerSelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					// Status port — workers only
					{
						From: []networkingv1.NetworkPolicyPeer{
							{PodSelector: &workerSelector},
						},
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &tcp, Port: &statusPort},
						},
					},
					// Resource API — open to all (Ingress/Route/port-forward)
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &tcp, Port: &resourcesPort},
						},
					},
				},
			},
		},
		// 2. Default-deny ingress for worker pods. Workers are pure clients —
		// nothing should connect to them. This mitigates RCE pivot attacks.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            mt.Name + "-worker-ingress-deny",
				Namespace:       mt.Namespace,
				OwnerReferences: owner,
				Labels:          map[string]string{"app.kubernetes.io/managed-by": "oc-mirror-operator"},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: workerSelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				// Empty ingress slice = deny all.
			},
		},
		// NOTE: A worker-egress policy was previously generated here that
		// restricted worker egress to "DNS + manager + 0.0.0.0/0 except
		// RFC1918". It was removed because it is impossible to know in
		// advance how a target cluster routes DNS (Service-CIDR vs Pod-CIDR
		// vs node-local resolver) and which non-RFC1918 ranges hold the
		// upstream registries. Operators that want to lock down worker egress
		// should author their own NetworkPolicy tailored to their cluster
		// topology — see README.md for an example.
	}

	for _, np := range policies {
		desired := np
		current := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, current, func() error {
			current.Labels = desired.Labels
			current.OwnerReferences = desired.OwnerReferences
			current.Spec = desired.Spec
			return nil
		}); err != nil {
			return fmt.Errorf("failed to ensure NetworkPolicy %s: %w", desired.Name, err)
		}
	}

	// Clean up obsolete NetworkPolicies that earlier versions of the operator
	// created. The worker-egress policy was removed because it cannot make
	// safe assumptions about a cluster's DNS topology; if it still exists
	// from a previous deployment, delete it now.
	obsolete := []string{mt.Name + "-worker-egress"}
	for _, name := range obsolete {
		stale := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mt.Namespace},
		}
		if err := r.Delete(ctx, stale); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete obsolete NetworkPolicy %s: %w", name, err)
		}
	}
	return nil
}

// handleDeletion runs the MirrorTarget finalizer logic: stops the manager
// Deployment, waits for all pods to terminate, then removes the finalizer.
func (r *MirrorTargetReconciler) handleDeletion(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
		return ctrl.Result{}, nil
	}

	// Delete the manager Deployment first so it stops spawning new pods.
	// Without this, the Deployment would keep recreating manager/worker pods
	// faster than the pod-level cleanup loop can delete them.
	dep := &appsv1.Deployment{}
	depKey := client.ObjectKey{Name: mt.Name + "-manager", Namespace: mt.Namespace}
	if err := r.Get(ctx, depKey, dep); err == nil {
		if err := r.Delete(ctx, dep); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(mt.Namespace),
		client.MatchingLabels{"mirrortarget": mt.Name}); err != nil {
		return ctrl.Result{}, err
	}
	// Issue deletes for any worker/manager pods still around. We only remove
	// the finalizer once all of them have actually disappeared, to
	// give them a chance to flush in-flight state and avoid leaking
	// pods that survive past MirrorTarget deletion.
	remaining := 0
	for _, pod := range podList.Items {
		if !pod.DeletionTimestamp.IsZero() {
			remaining++
			continue
		}
		if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
			l.Error(err, "Failed to delete pod", "pod", pod.Name)
			return ctrl.Result{}, err
		}
		remaining++
	}
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(mt, mirrorTargetFinalizer)
	if err := r.Update(ctx, mt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// creates cleanup Jobs for them (if cleanup-policy annotation is "Delete"),
// and tracks cleanup progress.
func (r *MirrorTargetReconciler) reconcileCleanup(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error { //nolint:unparam
	l := log.FromContext(ctx)

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
	stillPending := make([]string, 0, len(mt.Status.PendingCleanup))
	for _, name := range mt.Status.PendingCleanup {
		if r.isPendingCleanup(ctx, mt, name) {
			stillPending = append(stillPending, name)
		}
	}
	mt.Status.PendingCleanup = stillPending

	if len(removed) == 0 {
		// No new removals — update condition if pending cleanups just finished.
		if len(mt.Status.PendingCleanup) == 0 {
			for _, c := range mt.Status.Conditions {
				if c.Type == "Cleanup" && c.Reason == "CleanupInProgress" {
					setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionTrue, "CleanupComplete", "No pending cleanups", mt.Generation)
					break
				}
			}
		}
		return nil
	}

	cleanupPolicy := mt.Annotations[mirrorv1alpha1.CleanupPolicyAnnotation]
	if cleanupPolicy != mirrorv1alpha1.CleanupPolicyDelete {
		l.Info("ImageSets removed but cleanup-policy not set to Delete — skipping registry cleanup",
			"removed", removed, "annotation", cleanupPolicy)
		return nil
	}

	// Load consolidated per-MirrorTarget state once for all removed ImageSets.
	consolidatedState, loadErr := imagestate.LoadForTarget(ctx, r.Client, mt.Namespace, mt.Name)
	if loadErr != nil {
		l.Error(loadErr, "Failed to load consolidated state for cleanup")
		return nil
	}
	stateDirty := false

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

		created, dirty := r.partitionAndCreateCleanupJob(ctx, mt, isName, consolidatedState)
		if dirty {
			stateDirty = true
		}
		if created {
			mt.Status.PendingCleanup = append(mt.Status.PendingCleanup, isName)
		}
	}

	if stateDirty {
		if err := imagestate.SaveForTarget(ctx, r.Client, mt.Namespace, mt.Name, consolidatedState); err != nil {
			l.Error(err, "Failed to save consolidated state after cleanup partitioning")
		}
	}

	if len(mt.Status.PendingCleanup) > 0 {
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupInProgress",
			fmt.Sprintf("Cleaning up images for: %s", strings.Join(mt.Status.PendingCleanup, ", ")), mt.Generation)
	} else {
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionTrue, "CleanupComplete", "No pending cleanups", mt.Generation)
	}

	return nil
}

// isPendingCleanup checks whether the cleanup Job for the given ImageSet is
// still running (or needs to be re-created). Returns false when cleanup is done.
func (r *MirrorTargetReconciler) isPendingCleanup(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, name string) bool {
	l := log.FromContext(ctx)
	jobName := cleanupJobName(mt.Name, name)
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: mt.Namespace}, job)
	if errors.IsNotFound(err) {
		// Job gone — check whether the snapshot ConfigMap still exists.
		snapshotName := cleanupSnapshotCMName(mt.Name, name)
		snapshotCM := &corev1.ConfigMap{}
		if r.Get(ctx, client.ObjectKey{Name: snapshotName, Namespace: mt.Namespace}, snapshotCM) == nil {
			// Snapshot exists → job was deleted mid-run; re-create it.
			l.Info("Cleanup job gone but snapshot ConfigMap remains — re-creating job", "imageset", name)
			if createErr := r.createCleanupJob(ctx, mt, name, snapshotName); createErr != nil {
				l.Error(createErr, "Failed to re-create cleanup job", "imageset", name)
			}
			return true
		}
		l.Info("Cleanup job gone and snapshot ConfigMap absent — considering done", "imageset", name)
		return false
	}
	if err != nil {
		return true
	}
	if job.Status.Succeeded > 0 {
		l.Info("Cleanup completed successfully", "imageset", name, "job", jobName)
		return false
	}
	if job.Status.Failed > 0 {
		l.Error(nil, "Cleanup job failed — will retry", "imageset", name, "job", jobName)
		propagation := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation})
	}
	return true
}

// partitionAndCreateCleanupJob partitions the consolidated state for isName
// (exclusive vs shared), creates a snapshot ConfigMap, and launches a cleanup
// Job. Returns (jobCreated, stateDirty). The consolidated map is modified in place.
func (r *MirrorTargetReconciler) partitionAndCreateCleanupJob(
	ctx context.Context,
	mt *mirrorv1alpha1.MirrorTarget,
	isName string,
	consolidated imagestate.ImageState,
) (created, dirty bool) {
	l := log.FromContext(ctx)

	exclusiveState := make(imagestate.ImageState)
	for dest, entry := range consolidated {
		if entry.HasImageSet(isName) && len(entry.Refs) == 1 {
			exclusiveState[dest] = entry
		}
	}

	// Remove IS ref from entries shared with other ImageSets.
	for dest, entry := range consolidated {
		if entry.HasImageSet(isName) && len(entry.Refs) > 1 {
			entry.RemoveImageSet(isName)
			consolidated[dest] = entry
			dirty = true
		}
	}

	if len(exclusiveState) == 0 {
		l.Info("No exclusive images for removed ImageSet — skipping cleanup job", "imageset", isName)
		return false, dirty
	}

	snapshotName := cleanupSnapshotCMName(mt.Name, isName)
	if err := imagestate.SaveRaw(ctx, r.Client, mt.Namespace, snapshotName, exclusiveState); err != nil {
		l.Error(err, "Failed to create cleanup snapshot ConfigMap", "imageset", isName)
		return false, dirty
	}

	for dest := range exclusiveState {
		delete(consolidated, dest)
	}
	dirty = true

	l.Info("Creating cleanup job for removed ImageSet", "imageset", isName, "exclusiveImages", len(exclusiveState))
	if err := r.createCleanupJob(ctx, mt, isName, snapshotName); err != nil {
		l.Error(err, "Failed to create cleanup job", "imageset", isName)
		_ = r.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: snapshotName, Namespace: mt.Namespace}})
		return false, dirty
	}
	return true, dirty
}

// createCleanupJob creates a Kubernetes Job that deletes all images listed in
// the snapshot ConfigMap from the target registry and removes the snapshot.
func (r *MirrorTargetReconciler) createCleanupJob(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSetName, snapshotCMName string) error {
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
		"--configmap", snapshotCMName,
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
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
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
					ServiceAccountName: mt.Name + "-coordinator",
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
// Long target/imageset names are safely shortened by appending a SHA-256
// suffix to prevent name collisions caused by naive truncation.
func cleanupJobName(targetName, imageSetName string) string {
	const maxLen = 63
	const sumLen = 8
	h := sha256.New()
	_, _ = h.Write([]byte(targetName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(imageSetName))
	suffix := fmt.Sprintf("%x", h.Sum(nil))[:sumLen]
	body := "cleanup-" + targetName + "-" + imageSetName
	budget := maxLen - 1 - sumLen
	if len(body) > budget {
		body = body[:budget]
	}
	body = strings.TrimRight(body, "-")
	return body + "-" + suffix
}

// cleanupSnapshotCMName returns a deterministic name for the cleanup snapshot
// ConfigMap. Uses the same hash strategy as cleanupJobName but with the larger
// ConfigMap name budget (253 chars).
func cleanupSnapshotCMName(targetName, imageSetName string) string {
	const maxLen = 253
	const sumLen = 8
	h := sha256.New()
	_, _ = h.Write([]byte(targetName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(imageSetName))
	suffix := fmt.Sprintf("%x", h.Sum(nil))[:sumLen]
	body := targetName + "-cleanup-" + imageSetName
	budget := maxLen - 1 - sumLen
	if len(body) > budget {
		body = body[:budget]
	}
	body = strings.TrimRight(body, "-")
	return body + "-" + suffix
}

// SetupWithManager sets up the controller with the Manager.
//
// Watches:
//   - MirrorTarget (primary)
//   - ImageSet status changes → enqueue every MirrorTarget that lists the
//     ImageSet in spec.imageSets so the MirrorTarget's aggregated counters
//     stay in sync with per-ImageSet progress.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *MirrorTargetReconciler) ensureResourceAPI(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	name := "oc-mirror-resource-api"
	namespace := mt.Namespace

	if err := r.ensureResourceRBAC(ctx, mt); err != nil {
		return err
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		// Set owner reference to the MirrorTarget. Since this deployment is
		// shared per namespace, it will have multiple owner references (one for
		// each MirrorTarget). Kubernetes will garbage collect it when ALL
		// owners are gone.
		if err := controllerutil.SetOwnerReference(mt, deployment, r.Scheme); err != nil {
			return err
		}

		labels := map[string]string{"app": name}
		deployment.Labels = labels
		replicas := int32(1)
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: name,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: pointerTo(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "api",
							Image: os.Getenv("OPERATOR_IMAGE"),
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: pointerTo(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Args:  []string{"resource-api", "--namespace", namespace},
							Ports: []corev1.ContainerPort{{ContainerPort: 8081}},
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
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
	if err != nil {
		return fmt.Errorf("failed to ensure resource api deployment: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetOwnerReference(mt, svc, r.Scheme); err != nil {
			return err
		}
		labels := map[string]string{"app": name}
		svc.Labels = labels
		svc.Spec = corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 8081, TargetPort: intstr.FromInt(8081)}},
		}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) ensureResourceRBAC(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	name := "oc-mirror-resource-api"
	namespace := mt.Namespace
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetOwnerReference(mt, sa, r.Scheme)
	}); err != nil {
		return err
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := controllerutil.SetOwnerReference(mt, role, r.Scheme); err != nil {
			return err
		}
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"mirror.openshift.io"},
				Resources: []string{"mirrortargets", "imagesets"},
				Verbs:     []string{"get", "list", "watch"},
			},
		}
		return nil
	})
	if err != nil {
		return err
	}

	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		if err := controllerutil.SetOwnerReference(mt, rb, r.Scheme); err != nil {
			return err
		}
		rb.RoleRef = rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name}
		rb.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorTarget{}).
		Watches(
			&mirrorv1alpha1.ImageSet{},
			handler.EnqueueRequestsFromMapFunc(r.mirrorTargetsForImageSet),
		).
		Complete(r)
}

// mirrorTargetsForImageSet returns reconcile requests for every MirrorTarget
// in the same namespace that references the given ImageSet in spec.imageSets.
// Used to propagate per-ImageSet status changes (mirroring progress) onto
// the aggregated MirrorTarget counters.
func (r *MirrorTargetReconciler) mirrorTargetsForImageSet(ctx context.Context, obj client.Object) []reconcile.Request {
	is, ok := obj.(*mirrorv1alpha1.ImageSet)
	if !ok {
		return nil
	}
	var mtList mirrorv1alpha1.MirrorTargetList
	if err := r.List(ctx, &mtList, client.InNamespace(is.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, mt := range mtList.Items {
		for _, name := range mt.Spec.ImageSets {
			if name == is.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{Namespace: mt.Namespace, Name: mt.Name},
				})
				break
			}
		}
	}
	return requests
}

// aggregateImageSetStatus walks spec.imageSets and sums the per-ImageSet
// counters into MirrorTarget.Status. ImageSets that don't exist (yet) appear
// with Found=false and contribute zero. The per-ImageSet breakdown is sorted
// alphabetically for deterministic diffs.
func (r *MirrorTargetReconciler) aggregateImageSetStatus(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	names := make([]string, len(mt.Spec.ImageSets))
	copy(names, mt.Spec.ImageSets)
	sort.Strings(names)

	summaries := make([]mirrorv1alpha1.ImageSetStatusSummary, 0, len(names))
	var total, mirrored, pending, failed int

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
		total += is.Status.TotalImages
		mirrored += is.Status.MirroredImages
		pending += is.Status.PendingImages
		failed += is.Status.FailedImages
	}

	mt.Status.ImageSetStatuses = summaries
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

		spec, ok := route.Object["spec"].(map[string]interface{})
		if !ok {
			spec = make(map[string]interface{})
		}

		spec["to"] = map[string]interface{}{
			"kind": "Service",
			"name": svcName,
		}
		spec["port"] = map[string]interface{}{
			"targetPort": "resources",
		}
		spec["tls"] = map[string]interface{}{
			"termination":                   "edge",
			"insecureEdgeTerminationPolicy": "Redirect",
		}

		// Only set host when user explicitly provides it. If host is empty or unset,
		// we remove the field from our update map entirely. This allows OpenShift
		// to maintain the auto-generated host without triggering a "custom host"
		// permission check on the operator's service account.
		if mt.Spec.Expose != nil && mt.Spec.Expose.Host != "" {
			spec["host"] = mt.Spec.Expose.Host
		} else {
			delete(spec, "host")
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
		return fmt.Errorf("ingress exposure requires a host to be set in spec.expose.host")
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

// managerContainerEnv returns the env vars for the manager container.
// DOCKER_CONFIG is only set when an AuthSecret is configured, matching the
// volume mount.  Proxy and CA env vars are injected when configured.
func managerContainerEnv(mt *mirrorv1alpha1.MirrorTarget) []corev1.EnvVar {
	env := []corev1.EnvVar{
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
	}
	if mt.Spec.AuthSecret != "" {
		env = append(env, corev1.EnvVar{
			Name:  "DOCKER_CONFIG",
			Value: "/docker-config",
		})
	}
	env = append(env, proxyEnvVars(mt.Spec.Proxy)...)
	env = append(env, caBundleEnvVars(mt.Spec.CABundle)...)
	return env
}

// clusterNoProxy contains address patterns that always bypass the proxy so that
// pod-to-service traffic via cluster-internal FQDNs is never routed through an
// external proxy (e.g. the manager service at {target}-manager.{ns}.svc.cluster.local).
var clusterNoProxy = []string{
	"localhost",
	"127.0.0.1",
	".svc",
	".svc.cluster.local",
}

// proxyEnvVars returns HTTP/HTTPS/NO_PROXY env vars (upper and lower case) for
// the given proxy configuration.  Returns nil when cfg is nil.
// When a proxy is configured, clusterNoProxy entries are automatically prepended
// to NO_PROXY so that pod-to-service traffic bypasses the proxy by default.
func proxyEnvVars(cfg *mirrorv1alpha1.ProxyConfig) []corev1.EnvVar {
	if cfg == nil {
		return nil
	}
	var env []corev1.EnvVar
	if v := cfg.HTTPProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: v},
			corev1.EnvVar{Name: "http_proxy", Value: v},
		)
	}
	if v := cfg.HTTPSProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: v},
			corev1.EnvVar{Name: "https_proxy", Value: v},
		)
	}
	// When a proxy is configured, always inject cluster-internal NO_PROXY
	// defaults to prevent pod-to-service traffic from being proxied.
	// Also override KUBERNETES_SERVICE_HOST to use the FQDN instead of the
	// ClusterIP so that client-go's in-cluster config honours the NO_PROXY
	// FQDN suffix rule (.svc.cluster.local) when calling the Kubernetes API.
	if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
		noProxy := buildEffectiveNoProxy(cfg.NoProxy)
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: noProxy},
			corev1.EnvVar{Name: "no_proxy", Value: noProxy},
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: "kubernetes.default.svc.cluster.local"},
		)
	} else if cfg.NoProxy != "" {
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: cfg.NoProxy},
			corev1.EnvVar{Name: "no_proxy", Value: cfg.NoProxy},
		)
	}
	return env
}

// buildEffectiveNoProxy prepends clusterNoProxy to userNoProxy so that
// cluster-internal FQDNs always bypass the proxy.
func buildEffectiveNoProxy(userNoProxy string) string {
	base := strings.Join(clusterNoProxy, ",")
	if userNoProxy == "" {
		return base
	}
	return base + "," + userNoProxy
}

// caBundleEnvVars returns the SSL_CERT_FILE env var pointing to the mounted CA
// bundle.  Returns nil when ref is nil.
func caBundleEnvVars(ref *mirrorv1alpha1.CABundleRef) []corev1.EnvVar {
	if ref == nil {
		return nil
	}
	key := ref.Key
	if key == "" {
		key = "ca-bundle.crt"
	}
	return []corev1.EnvVar{
		{Name: "SSL_CERT_FILE", Value: "/run/secrets/ca/" + key},
	}
}

// managerContainerVolumeMounts returns the volume mounts for the manager
// container. The dockerconfig mount is only present when an AuthSecret is
// configured to avoid pod admission failures on empty secret references.
// The ca-bundle mount is added when a CABundle is configured.
func managerContainerVolumeMounts(mt *mirrorv1alpha1.MirrorTarget) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if mt.Spec.AuthSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "dockerconfig",
			MountPath: "/docker-config",
			ReadOnly:  true,
		})
	}
	if mt.Spec.CABundle != nil {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "ca-bundle",
			MountPath: "/run/secrets/ca",
			ReadOnly:  true,
		})
	}
	return mounts
}

// managerPodVolumes returns the volumes for the manager pod, gated on the
// presence of an AuthSecret and/or a CABundle.
func managerPodVolumes(mt *mirrorv1alpha1.MirrorTarget) []corev1.Volume {
	var volumes []corev1.Volume
	if mt.Spec.AuthSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "dockerconfig",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mt.Spec.AuthSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
	}
	if mt.Spec.CABundle != nil {
		key := mt.Spec.CABundle.Key
		if key == "" {
			key = "ca-bundle.crt"
		}
		volumes = append(volumes, corev1.Volume{
			Name: "ca-bundle",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: mt.Spec.CABundle.ConfigMapName,
					},
					Items: []corev1.KeyToPath{
						{Key: key, Path: key},
					},
				},
			},
		})
	}
	return volumes
}
