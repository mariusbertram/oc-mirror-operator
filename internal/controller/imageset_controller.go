package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
)

// ImageSetReconciler reconciles a ImageSet object
type ImageSetReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	MirrorClient *mirrorclient.MirrorClient
	Collector    *mirror.Collector
	StateManager *state.StateManager
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/finalizers,verbs=update
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets,verbs=get;list;watch

func (r *ImageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	is := &mirrorv1alpha1.ImageSet{}
	if err := r.Get(ctx, req.NamespacedName, is); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Get MirrorTarget
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, types.NamespacedName{Name: is.Spec.TargetRef, Namespace: is.Namespace}, mt); err != nil {
		if errors.IsNotFound(err) {
			l.Error(err, "MirrorTarget not found", "targetRef", is.Spec.TargetRef)
			setCondition(&is.Status.Conditions, "Ready", metav1.ConditionFalse, "MirrorTargetNotFound", "MirrorTarget "+is.Spec.TargetRef+" not found")
			_ = r.Status().Update(ctx, is)
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Load Metadata from registry
	metaRepo := fmt.Sprintf("%s/oc-mirror-metadata", mt.Spec.Registry)
	meta, _, err := r.StateManager.ReadMetadata(ctx, metaRepo, "latest")
	if err != nil {
		l.Error(err, "Failed to read metadata from registry")
		// Continue with empty meta for now
		meta = &state.Metadata{MirroredImages: make(map[string]string)}
	}

	// 3. Generate Soll-Liste if empty or if spec has changed since last collection
	if len(is.Status.TargetImages) == 0 || is.Status.ObservedGeneration != is.Generation {
		l.Info("Generating image list for ImageSet")
		images, err := r.Collector.CollectTargetImages(ctx, &is.Spec, mt, meta)
		if err != nil {
			setCondition(&is.Status.Conditions, "Ready", metav1.ConditionFalse, "CollectionFailed", err.Error())
			_ = r.Status().Update(ctx, is)
			return ctrl.Result{}, err
		}

		targetStatus := make([]mirrorv1alpha1.TargetImageStatus, len(images))
		for i, img := range images {
			targetStatus[i] = mirrorv1alpha1.TargetImageStatus{
				Source:      img.Source,
				Destination: img.Destination,
				State:       img.State,
			}
		}

		is.Status.TargetImages = targetStatus
		is.Status.TotalImages = len(targetStatus)
		// Calculate mirrored count from generated list
		mirrored := 0
		for _, img := range targetStatus {
			if img.State == "Mirrored" {
				mirrored++
			}
		}
		is.Status.MirroredImages = mirrored
		is.Status.ObservedGeneration = is.Generation
		setCondition(&is.Status.Conditions, "Ready", metav1.ConditionTrue, "Collected", fmt.Sprintf("Collected %d images", len(targetStatus)))
		if err := r.Status().Update(ctx, is); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.MirrorClient = mirrorclient.NewMirrorClient(nil, "")
	r.Collector = mirror.NewCollector(r.MirrorClient)
	r.StateManager = state.New(r.MirrorClient)

	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.ImageSet{}).
		Watches(
			&mirrorv1alpha1.MirrorTarget{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				// Requeue all ImageSets that reference this MirrorTarget
				imageSets := &mirrorv1alpha1.ImageSetList{}
				if err := mgr.GetClient().List(ctx, imageSets, client.InNamespace(obj.GetNamespace())); err != nil {
					return nil
				}
				var requests []reconcile.Request
				for _, is := range imageSets.Items {
					if is.Spec.TargetRef == obj.GetName() {
						requests = append(requests, reconcile.Request{
							NamespacedName: types.NamespacedName{
								Name:      is.Name,
								Namespace: is.Namespace,
							},
						})
					}
				}
				return requests
			}),
		).
		Complete(r)
}
