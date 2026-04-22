package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog/builder"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// ImageSetReconciler reconciles a ImageSet object
type ImageSetReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	MirrorClient    *mirrorclient.MirrorClient
	Collector       *mirror.Collector
	CatalogBuildMgr *builder.CatalogBuildManager
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/finalizers,verbs=update
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *ImageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	is := &mirrorv1alpha1.ImageSet{}
	if err := r.Get(ctx, req.NamespacedName, is); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Find the MirrorTarget(s) that reference this ImageSet via spec.imageSets.
	mt, err := r.findOwningMirrorTarget(ctx, is)
	if err != nil {
		l.Info("No MirrorTarget references this ImageSet", "imageSet", is.Name, "reason", err.Error())
		setCondition(&is.Status.Conditions, "Ready", metav1.ConditionFalse, "Unbound", err.Error())
		_ = r.Status().Update(ctx, is)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// 2. Compute poll state. Determines whether a periodic upstream re-check is due.
	pollInterval := 24 * time.Hour
	if mt.Spec.PollInterval != nil && mt.Spec.PollInterval.Duration > 0 {
		pollInterval = mt.Spec.PollInterval.Duration
		if pollInterval < 1*time.Hour {
			pollInterval = 1 * time.Hour
		}
	}
	pollingEnabled := mt.Spec.PollInterval == nil || mt.Spec.PollInterval.Duration > 0
	pollExpired := false
	if pollingEnabled && is.Status.LastSuccessfulPollTime != nil {
		pollExpired = time.Since(is.Status.LastSuccessfulPollTime.Time) >= pollInterval
	}

	// 3. Ensure a CatalogBuildJob exists for each configured operator catalog.
	// This is done before the expensive image-list collection so that catalog
	// builds start immediately without waiting for release image traversal.
	if err := r.reconcileCatalogBuildJobs(ctx, is, mt, pollExpired); err != nil {
		l.Error(err, "Failed to reconcile catalog build jobs")
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildFailed", err.Error())
		_ = r.Status().Update(ctx, is)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Build per-reconcile mirror client that honours the target's insecure setting.
	// The struct-level r.Collector is used as fallback when target is not insecure.
	collector := r.Collector
	if mt.Spec.Insecure {
		host := mt.Spec.Registry
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		mc := mirrorclient.NewMirrorClient([]string{host}, os.Getenv("DOCKER_CONFIG"))
		collector = mirror.NewCollector(mc)
	}

	// 4. Generate target image list for releases and additional images.
	// Operator catalog images are handled by CatalogBuildJobs (step 3) and are
	// NOT resolved in-memory here to avoid downloading multi-GB catalog layers
	// inside the controller pod.
	existingState, loadErr := imagestate.Load(ctx, r.Client, is.Namespace, is.Name)
	if loadErr != nil {
		l.Error(loadErr, "Failed to load image state ConfigMap")
	}

	needsCollection := len(existingState) == 0 ||
		is.Status.ObservedGeneration != is.Generation ||
		pollExpired

	if needsCollection {
		if pollExpired {
			l.Info("Poll interval expired, re-collecting upstream content",
				"lastPoll", is.Status.LastSuccessfulPollTime.Time,
				"interval", pollInterval)
			// Reset Failed entries to Pending so they get retried.
			for dest, entry := range existingState {
				if entry.State == "Failed" {
					existingState[dest] = &imagestate.ImageEntry{
						Source: entry.Source,
						State:  "Pending",
					}
				}
			}
		} else {
			l.Info("Generating image list for ImageSet")
		}

		images, err := collector.CollectTargetImages(ctx, &is.Spec, mt, nil)
		if err != nil {
			setCondition(&is.Status.Conditions, "Ready", metav1.ConditionFalse, "CollectionFailed", err.Error())
			_ = r.Status().Update(ctx, is)
			return ctrl.Result{}, err
		}

		// Build new state, preserving Mirrored entries from the existing
		// ConfigMap so already-mirrored images are not re-queued.
		newState := make(imagestate.ImageState, len(images))
		for _, img := range images {
			if existing, ok := existingState[img.Destination]; ok && existing.State != "Pending" {
				newState[img.Destination] = existing
			} else {
				newState[img.Destination] = &imagestate.ImageEntry{
					Source: img.Source,
					State:  img.State,
				}
			}
		}

		if err := imagestate.Save(ctx, r.Client, is.Namespace, is, newState); err != nil {
			l.Error(err, "Failed to save image state ConfigMap")
		}
		existingState = newState
		is.Status.ObservedGeneration = is.Generation
		now := metav1.Now()
		is.Status.LastSuccessfulPollTime = &now
	}

	// Seed LastSuccessfulPollTime for existing ImageSets that were reconciled
	// before the polling feature was added.
	if is.Status.LastSuccessfulPollTime == nil && len(existingState) > 0 {
		now := metav1.Now()
		is.Status.LastSuccessfulPollTime = &now
	}

	total, mirrored, pending, failed := imagestate.Counts(existingState)
	is.Status.TotalImages = total
	is.Status.MirroredImages = mirrored
	is.Status.PendingImages = pending
	is.Status.FailedImages = failed
	setCondition(&is.Status.Conditions, "Ready", metav1.ConditionTrue, "Collected", fmt.Sprintf("Collected %d images", total))
	if err := r.Status().Update(ctx, is); err != nil {
		return ctrl.Result{}, err
	}

	if pollingEnabled {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// findOwningMirrorTarget returns the single MirrorTarget that references this
// ImageSet in its spec.imageSets list. Returns an error when zero or more than
// one MirrorTarget references the ImageSet (ambiguous ownership).
func (r *ImageSetReconciler) findOwningMirrorTarget(ctx context.Context, is *mirrorv1alpha1.ImageSet) (*mirrorv1alpha1.MirrorTarget, error) {
	mtList := &mirrorv1alpha1.MirrorTargetList{}
	if err := r.List(ctx, mtList, client.InNamespace(is.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list MirrorTargets: %w", err)
	}

	var matches []*mirrorv1alpha1.MirrorTarget
	for i := range mtList.Items {
		for _, name := range mtList.Items[i].Spec.ImageSets {
			if name == is.Name {
				matches = append(matches, &mtList.Items[i])
				break
			}
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no MirrorTarget references ImageSet %s", is.Name)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		return nil, fmt.Errorf("ImageSet %s is referenced by multiple MirrorTargets (%s); each ImageSet may only be in one MirrorTarget", is.Name, strings.Join(names, ", "))
	}
}

// reconcileCatalogBuildJobs ensures a Kubernetes Job exists for each operator
// catalog entry in the ImageSet spec and surfaces the aggregate status as a
// CatalogReady condition.
func (r *ImageSetReconciler) reconcileCatalogBuildJobs(
	ctx context.Context,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	pollExpired bool,
) error {
	l := log.FromContext(ctx)

	operators := is.Spec.Mirror.Operators
	if len(operators) == 0 {
		return nil
	}

	// Compute a build signature from operator image + packages so we detect
	// when a rebuild is needed (operator image upgrade, package list change).
	buildSig := r.CatalogBuildMgr.BuildSignature(operators)
	lastSig := ""
	if is.Annotations != nil {
		lastSig = is.Annotations["mirror.openshift.io/catalog-build-sig"]
	}

	catalogNeedsRebuild := lastSig != "" && lastSig != buildSig
	if catalogNeedsRebuild {
		l.Info("Catalog build signature changed, forcing rebuild", "old", lastSig, "new", buildSig)
	}
	// Force catalog rebuild when poll interval expired — upstream catalog images
	// (e.g. redhat-operator-index:v4.21) may have been updated in-place.
	if pollExpired && !catalogNeedsRebuild {
		l.Info("Poll interval expired, forcing catalog rebuild")
		catalogNeedsRebuild = true
	}

	// If the CatalogReady condition is already True AND the signature hasn't
	// changed, don't recreate jobs that were cleaned up by TTL.
	catalogAlreadyReady := false
	if !catalogNeedsRebuild {
		for _, c := range is.Status.Conditions {
			if c.Type == "CatalogReady" && c.Status == metav1.ConditionTrue {
				catalogAlreadyReady = true
				break
			}
		}
	}

	allSucceeded := true
	anyFailed := false

	for _, op := range operators {
		if op.Catalog == "" {
			continue
		}

		// Collect package names for this catalog entry.
		var packages []string
		if !op.Full {
			for _, p := range op.Packages {
				packages = append(packages, p.Name)
			}
		}

		// Derive the target catalog image reference.
		targetRef := catalogTargetRef(mt.Spec.Registry, op)

		jobName := builder.JobName(is.Name, op.Catalog)
		phase, err := builder.GetBuildJobStatus(ctx, r.Client, jobName, is.Namespace)
		if err != nil {
			return err
		}

		// If a rebuild is needed, delete the old Job first.
		if catalogNeedsRebuild && phase != builder.JobPhaseNotFound {
			l.Info("Deleting stale CatalogBuildJob for rebuild", "job", jobName)
			if delErr := builder.DeleteBuildJob(ctx, r.Client, jobName, is.Namespace); delErr != nil {
				l.Error(delErr, "Failed to delete stale CatalogBuildJob", "job", jobName)
			}
			phase = builder.JobPhaseNotFound
		}

		// If the job was TTL-cleaned but catalog was already built, treat as succeeded.
		if phase == builder.JobPhaseNotFound && catalogAlreadyReady {
			l.Info("CatalogBuildJob already completed (TTL-cleaned)", "job", jobName)
			continue
		}

		// Ensure the Job exists (no-op if it already does).
		if phase == builder.JobPhaseNotFound {
			if err := r.CatalogBuildMgr.EnsureCatalogBuildJob(ctx, r.Client, is, mt, op.Catalog, targetRef, packages); err != nil {
				return fmt.Errorf("failed to ensure CatalogBuildJob for %s: %w", op.Catalog, err)
			}
			phase, err = builder.GetBuildJobStatus(ctx, r.Client, jobName, is.Namespace)
			if err != nil {
				return err
			}
		}

		l.Info("CatalogBuildJob status", "job", jobName, "phase", phase)

		switch phase {
		case builder.JobPhaseSucceeded:
			// good
		case builder.JobPhaseFailed:
			anyFailed = true
			allSucceeded = false
		default:
			allSucceeded = false
		}
	}

	// Store build signature in annotation.
	if allSucceeded {
		if is.Annotations == nil {
			is.Annotations = make(map[string]string)
		}
		is.Annotations["mirror.openshift.io/catalog-build-sig"] = buildSig
		if err := r.Update(ctx, is); err != nil {
			l.Error(err, "Failed to update catalog build signature annotation")
		}
	}

	switch {
	case anyFailed:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildFailed", "one or more catalog build jobs failed")
	case allSucceeded:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionTrue, "CatalogBuildSucceeded", "all catalog images built successfully")
	default:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildRunning", "catalog build jobs are still running")
	}

	return r.Status().Update(ctx, is)
}

// catalogTargetRef builds the target image reference for a filtered catalog image.
// It prefers Operator.TargetCatalog if set; otherwise derives a path from the
// source catalog name and appends the TargetTag (defaulting to "latest").
func catalogTargetRef(registry string, op mirrorv1alpha1.Operator) string {
	tag := op.TargetTag
	if tag == "" {
		// Default to the source catalog's tag (e.g. "v4.21" from "index:v4.21").
		// Skip digest references (contain "@").
		if !strings.Contains(op.Catalog, "@") {
			if i := strings.LastIndex(op.Catalog, ":"); i >= 0 && !strings.Contains(op.Catalog[i:], "/") {
				tag = op.Catalog[i+1:]
			}
		}
	}
	if tag == "" {
		tag = "latest"
	}
	if op.TargetCatalog != "" {
		return fmt.Sprintf("%s/%s:%s", registry, op.TargetCatalog, tag)
	}
	// Derive a safe path from the source catalog: strip registry prefix, keep image name.
	parts := strings.SplitN(op.Catalog, "/", 2)
	path := op.Catalog
	if len(parts) == 2 {
		path = parts[1]
	}
	// Remove tag/digest from path.
	if i := strings.IndexAny(path, ":@"); i >= 0 {
		path = path[:i]
	}
	return fmt.Sprintf("%s/%s:%s", registry, path, tag)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	authDir := os.Getenv("DOCKER_CONFIG")
	r.MirrorClient = mirrorclient.NewMirrorClient(nil, authDir)
	r.Collector = mirror.NewCollector(r.MirrorClient)
	r.CatalogBuildMgr = builder.New()

	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.ImageSet{}).
		Owns(&batchv1.Job{}).
		Watches(
			&mirrorv1alpha1.MirrorTarget{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				mt, ok := obj.(*mirrorv1alpha1.MirrorTarget)
				if !ok {
					return nil
				}
				// Requeue all ImageSets listed in this MirrorTarget's spec.imageSets.
				var requests []reconcile.Request
				for _, isName := range mt.Spec.ImageSets {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      isName,
							Namespace: mt.Namespace,
						},
					})
				}
				return requests
			}),
		).
		Complete(r)
}
