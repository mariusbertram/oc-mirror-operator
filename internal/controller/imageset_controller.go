package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/mariusbertram/ocp-mirror/api/v1alpha1"
	"github.com/mariusbertram/ocp-mirror/pkg/mirror"
)

// ImageSetReconciler reconciles a ImageSet object
type ImageSetReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	MirrorClient *mirror.MirrorClient
	Collector    *mirror.Collector
	Pool         *mirror.WorkerPool
	mu           sync.Mutex
	ActiveTasks  map[string]bool // map[ImageSetKey:Index]bool
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
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Generate Soll-Liste if empty
	if len(is.Status.TargetImages) == 0 {
		l.Info("Generating image list for ImageSet")
		images, err := r.Collector.CollectTargetImages(ctx, &is.Spec, mt)
		if err != nil {
			return ctrl.Result{}, err
		}

		targetStatus := make([]mirrorv1alpha1.TargetImageStatus, len(images))
		for i, img := range images {
			targetStatus[i] = mirrorv1alpha1.TargetImageStatus{
				Source:      img.Source,
				Destination: img.Destination,
				State:       "Pending",
			}
		}

		is.Status.TargetImages = targetStatus
		is.Status.TotalImages = len(targetStatus)
		is.Status.MirroredImages = 0
		if err := r.Status().Update(ctx, is); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3. Submit Pending images to the worker pool
	isKey := req.NamespacedName.String()
	numSubmitted := 0
	for i := range is.Status.TargetImages {
		img := &is.Status.TargetImages[i]
		if img.State == "Pending" {
			taskKey := fmt.Sprintf("%s:%d", isKey, i)
			r.mu.Lock()
			if !r.ActiveTasks[taskKey] {
				r.ActiveTasks[taskKey] = true
				r.Pool.Submit(mirror.Task{
					Source:      img.Source,
					Destination: img.Destination,
					ImageSetKey: isKey,
					ImageIndex:  i,
				})
				numSubmitted++
			}
			r.mu.Unlock()
		}
		// Limit submission rate per reconciliation to avoid overwhelming the pool
		if numSubmitted >= 10 {
			break
		}
	}

	if numSubmitted > 0 {
		l.Info("Submitted images to worker pool", "count", numSubmitted)
	}

	// 4. Update overall status conditions
	// (Simplified for now)
	if is.Status.MirroredImages == is.Status.TotalImages && is.Status.TotalImages > 0 {
		l.Info("Mirroring complete", "total", is.Status.TotalImages)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// StartResultProcessor starts a background loop to process worker pool results
func (r *ImageSetReconciler) StartResultProcessor(ctx context.Context) {
	l := log.FromContext(ctx).WithName("result-processor")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case res, ok := <-r.Pool.Results():
				if !ok {
					return
				}
				r.processTaskResult(ctx, res, l)
			}
		}
	}()
}

func (r *ImageSetReconciler) processTaskResult(ctx context.Context, res mirror.TaskResult, l log.Logr) {
	// 1. Mark task as not active
	taskKey := fmt.Sprintf("%s:%d", res.Task.ImageSetKey, res.Task.ImageIndex)
	r.mu.Lock()
	delete(r.ActiveTasks, taskKey)
	r.mu.Unlock()

	// 2. Update ImageSet status
	namespacedName := types.NamespacedName{}
	fmt.Sscanf(res.Task.ImageSetKey, "%s/%s", &namespacedName.Namespace, &namespacedName.Name)
	// Actually, req.NamespacedName.String() is "namespace/name"
	parts := fmt.Split(res.Task.ImageSetKey, "/")
	if len(parts) == 2 {
		namespacedName.Namespace = parts[0]
		namespacedName.Name = parts[1]
	}

	is := &mirrorv1alpha1.ImageSet{}
	if err := r.Get(ctx, namespacedName, is); err != nil {
		l.Error(err, "Failed to get ImageSet to update status", "key", res.Task.ImageSetKey)
		return
	}

	if res.Task.ImageIndex >= len(is.Status.TargetImages) {
		l.Error(fmt.Errorf("index out of bounds"), "ImageIndex out of bounds", "index", res.Task.ImageIndex)
		return
	}

	img := &is.Status.TargetImages[res.Task.ImageIndex]
	if res.Error != nil {
		img.State = "Failed"
		img.LastError = res.Error.Error()
	} else if res.IsSkipped {
		img.State = "Mirrored"
		is.Status.MirroredImages++
	} else {
		img.State = "Mirrored"
		is.Status.MirroredImages++
	}

	if err := r.Status().Update(ctx, is); err != nil {
		l.Error(err, "Failed to update ImageSet status", "key", res.Task.ImageSetKey)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.MirrorClient = mirror.NewMirrorClient()
	r.Collector = mirror.NewCollector(r.MirrorClient)
	r.ActiveTasks = make(map[string]bool)
	r.Pool = mirror.NewWorkerPool(context.Background(), r.MirrorClient, 10)

	// Start result processor
	r.StartResultProcessor(context.Background())

	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.ImageSet{}).
		Complete(r)
}
