package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		setCondition(&is.Status.Conditions, "Ready", metav1.ConditionFalse, "Unbound", err.Error(), is.Generation)
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
	// The job triggers when (a) all operator-origin imagestate entries are
	// Mirrored, OR (b) the user sets the "mirror.openshift.io/recollect"
	// annotation on the ImageSet (one-shot, cleared after recollection).
	if err := r.reconcileCatalogBuildJobs(ctx, is, mt, pollExpired); err != nil {
		l.Error(err, "Failed to reconcile catalog build jobs")
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildFailed", err.Error(), is.Generation)
		_ = r.Status().Update(ctx, is)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Image-list resolution + state ConfigMap + ImageSet.Status counts are
	// owned by the per-MirrorTarget Manager pod. The Manager has the
	// upstream registry credentials (via DOCKER_CONFIG) and runs the only
	// writer to imagestate, avoiding races with the controller. The
	// controller's job here is limited to:
	//   - validating bindings (done above)
	//   - keeping CatalogBuild jobs in sync (done above)
	//   - requeueing on pollInterval so MirrorTarget changes propagate
	//
	// The Manager publishes:
	//   - imagestate ConfigMap (per ImageSet)
	//   - is.Status.{TotalImages,MirroredImages,PendingImages,FailedImages}
	//   - is.Status.ObservedGeneration / LastSuccessfulPollTime
	//   - is.Status.Conditions for "Ready"
	//
	// The controller leaves these fields alone.
	_ = pollExpired // currently used only by reconcileCatalogBuildJobs

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

	// Gate: only launch / re-launch catalog build jobs when (a) the
	// "mirror.openshift.io/recollect" annotation requests it as a one-shot,
	// or (b) all operator-origin entries in the per-ImageSet imagestate
	// ConfigMap have reached the "Mirrored" state. Building the filtered
	// catalog before bundle images are present in the target registry would
	// produce a catalog that references unresolved digests.
	_, recollectRequested := is.Annotations[mirrorv1alpha1.RecollectAnnotation]
	operatorMirroringComplete, knowState := operatorImagesMirrored(ctx, r.Client, is)
	gateOpen := recollectRequested || operatorMirroringComplete
	if !gateOpen {
		if knowState {
			l.Info("Catalog build deferred: operator images still mirroring",
				"imageSet", is.Name)
		} else {
			l.Info("Catalog build deferred: imagestate not yet populated by manager",
				"imageSet", is.Name)
		}
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse,
			"WaitingForOperatorMirror",
			"waiting for operator bundle images to be mirrored before building filtered catalog",
			is.Generation)
		return r.Status().Update(ctx, is)
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

	// Persist the new build signature IMMEDIATELY so that concurrent/subsequent
	// reconcile loops do not keep seeing a mismatch and endlessly delete+recreate jobs.
	if catalogNeedsRebuild {
		if is.Annotations == nil {
			is.Annotations = make(map[string]string)
		}
		is.Annotations["mirror.openshift.io/catalog-build-sig"] = buildSig
		if err := r.Update(ctx, is); err != nil {
			return fmt.Errorf("failed to persist catalog build signature: %w", err)
		}
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

	switch {
	case anyFailed:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildFailed", "one or more catalog build jobs failed", is.Generation)
	case allSucceeded:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionTrue, "CatalogBuildSucceeded", "all catalog images built successfully", is.Generation)
		// Honoring the recollect annotation is one-shot: clear it after a
		// successful catalog build so subsequent reconciles use the
		// signature-based gating again.
		if recollectRequested {
			if is.Annotations != nil {
				delete(is.Annotations, mirrorv1alpha1.RecollectAnnotation)
				if err := r.Update(ctx, is); err != nil {
					l.Error(err, "Failed to clear recollect annotation")
				}
			}
		}
	default:
		setCondition(&is.Status.Conditions, "CatalogReady", metav1.ConditionFalse, "CatalogBuildRunning", "catalog build jobs are still running", is.Generation)
	}

	return r.Status().Update(ctx, is)
}

// operatorImagesMirrored returns (complete, knowState).
// complete = true when every operator-origin entry in the imagestate ConfigMap
// is either "Mirrored" or has PermanentlyFailed=true. PermanentlyFailed images
// are treated as done so that a single unavailable upstream image does not
// block the catalog build indefinitely, even while the manager periodically
// retries them. The failure is surfaced via ImageSet.status.failedImageDetails.
// knowState = false when no imagestate ConfigMap exists yet (manager hasn't
// performed initial resolution); the gate is closed in that case so we don't
// kick off a build with zero source data.
func operatorImagesMirrored(ctx context.Context, c client.Client, is *mirrorv1alpha1.ImageSet) (bool, bool) {
	state, err := imagestate.Load(ctx, c, is.Namespace, is.Name)
	if err != nil || len(state) == 0 {
		return false, false
	}
	hasOperator := false
	for _, e := range state {
		if e == nil || e.Origin != imagestate.OriginOperator {
			continue
		}
		hasOperator = true
		if e.State != "Mirrored" && !e.PermanentlyFailed {
			return false, true
		}
	}
	return hasOperator, true
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
	bm, err := builder.New()
	if err != nil {
		return fmt.Errorf("init catalog build manager: %w", err)
	}
	r.CatalogBuildMgr = bm

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
		// Watch the per-ImageSet imagestate ConfigMap (suffixed "-images")
		// owned by the manager pod. When the manager flips the last
		// operator-origin entry to Mirrored we want to immediately re-evaluate
		// the catalog-build gate instead of waiting for the next pollInterval.
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				name := obj.GetName()
				const suffix = "-images"
				if !strings.HasSuffix(name, suffix) {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      strings.TrimSuffix(name, suffix),
						Namespace: obj.GetNamespace(),
					},
				}}
			}),
		).
		Complete(r)
}

// createPartialCleanupJob saves removed images to a temporary ConfigMap and
// creates a cleanup Job that deletes them from the target registry.
// The ConfigMap and Job are named deterministically per ImageSet generation
// so that rapid spec changes produce distinct, immutable cleanup batches.
func (r *ImageSetReconciler) createPartialCleanupJob(
	ctx context.Context,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	removedImages imagestate.ImageState,
) error {
	l := log.FromContext(ctx)

	cmName := fmt.Sprintf("cleanup-partial-%s-gen%d", is.Name, is.Generation)
	jobName := cmName

	// Save removed images to the temporary ConfigMap.
	if err := imagestate.SaveRaw(ctx, r.Client, is.Namespace, cmName, removedImages); err != nil {
		return fmt.Errorf("save partial cleanup state: %w", err)
	}

	// Set OwnerReference on the cleanup ConfigMap to the ImageSet so it is
	// garbage-collected automatically when the ImageSet is deleted, even if
	// the cleanup Job is removed earlier (e.g. by TTLSecondsAfterFinished).
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: is.Namespace}, cm); err == nil {
		if err := controllerutil.SetControllerReference(is, cm, r.Scheme); err == nil {
			if updErr := r.Update(ctx, cm); updErr != nil && !errors.IsConflict(updErr) {
				l.Error(updErr, "failed to set OwnerReference on cleanup ConfigMap", "cm", cmName)
			}
		}
	}

	// Check if Job already exists.
	existing := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: is.Namespace}, existing); err == nil {
		l.Info("Partial cleanup job already exists", "job", jobName)
		return nil
	}

	backoffLimit := int32(3)
	ttlAfterFinished := int32(600)

	labels := map[string]string{
		"app.kubernetes.io/managed-by":     "oc-mirror-operator",
		"mirror.openshift.io/cleanup-type": "partial",
		"mirror.openshift.io/imageset":     is.Name,
	}

	insecureFlag := ""
	if mt.Spec.Insecure {
		insecureFlag = "--insecure"
	}
	args := []string{
		"cleanup",
		"--configmap", cmName,
		"--namespace", is.Namespace,
		"--registry", mt.Spec.Registry,
	}
	if insecureFlag != "" {
		args = append(args, insecureFlag)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: is.Namespace,
			Labels:    labels,
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
					Containers: []corev1.Container{{
						Name:  "cleanup",
						Image: os.Getenv("OPERATOR_IMAGE"),
						Args:  args,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: pointerTo(false),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						Env:          partialCleanupEnv(mt),
						VolumeMounts: partialCleanupVolumeMounts(mt),
					}},
					Volumes: partialCleanupVolumes(mt),
				},
			},
		},
	}

	l.Info("Creating partial cleanup job", "job", jobName, "images", len(removedImages))
	return r.Create(ctx, job)
}

// partialCleanupEnv returns the env for the partial-cleanup container.
// DOCKER_CONFIG is only exported when an AuthSecret is configured.
func partialCleanupEnv(mt *mirrorv1alpha1.MirrorTarget) []corev1.EnvVar {
	if mt.Spec.AuthSecret == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: "DOCKER_CONFIG", Value: "/docker-config"}}
}

func partialCleanupVolumeMounts(mt *mirrorv1alpha1.MirrorTarget) []corev1.VolumeMount {
	if mt.Spec.AuthSecret == "" {
		return nil
	}
	return []corev1.VolumeMount{{Name: "docker-config", MountPath: "/docker-config", ReadOnly: true}}
}

func partialCleanupVolumes(mt *mirrorv1alpha1.MirrorTarget) []corev1.Volume {
	if mt.Spec.AuthSecret == "" {
		return nil
	}
	return []corev1.Volume{{
		Name: "docker-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: mt.Spec.AuthSecret,
				Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
			},
		},
	}}
}
