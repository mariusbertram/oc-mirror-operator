package controller

import (
	"context"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
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
							Image: os.Getenv("CONTROLLER_IMAGE"), // Ensure this is set or use a default
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
									Name:  "CONTROLLER_IMAGE",
									Value: os.Getenv("CONTROLLER_IMAGE"),
								},
							},
							Resources: mt.Spec.Manager.Resources,
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

	setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionTrue, "DeploymentReady", "Manager deployment is active")
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

func pointerTo[T any](v T) *T {
	return &v
}
