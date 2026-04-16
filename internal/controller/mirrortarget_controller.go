package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/mariusbertram/ocp-mirror/api/v1alpha1"
)

// MirrorTargetReconciler reconciles a MirrorTarget object
type MirrorTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *MirrorTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, req.NamespacedName, mt); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Validate AuthSecret if provided
	if mt.Spec.AuthSecret != "" {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: mt.Spec.AuthSecret, Namespace: mt.Namespace}, secret)
		if err != nil {
			if errors.IsNotFound(err) {
				l.Error(err, "AuthSecret not found", "secret", mt.Spec.AuthSecret)
				// Update status with error condition
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// Update status condition
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Validated",
		Message:            "MirrorTarget is valid",
		LastTransitionTime: metav1.Now(),
	}
	// (Actually we should update conditions slice correctly)
	mt.Status.Conditions = []metav1.Condition{condition}
	if err := r.Status().Update(ctx, mt); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MirrorTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorTarget{}).
		Complete(r)
}
