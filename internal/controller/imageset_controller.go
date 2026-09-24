package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilretry "k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	ocmetrics "github.com/mariusbertram/oc-mirror-operator/pkg/metrics"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog/builder"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
)

// ImageSetReconciler reconciles a ImageSet object
type ImageSetReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	CatalogBuildMgr *builder.CatalogBuildManager
}

const conditionCatalogReady = "CatalogReady"

// reasonWaitingForOperatorMirror is the CatalogReady condition reason used
// whenever a catalog build is deferred because operator bundle images
// aren't (yet) confirmed mirrored — including when there's no resolved
// digest to pin the build to. Shared with tests, which assert on it too.
const reasonWaitingForOperatorMirror = "WaitingForOperatorMirror"

// catalogBuildSigAnnotation caches the signature (operator image + packages)
// of the last catalog build, so a spec change can be detected without
// recomputing it against a stale build.
const catalogBuildSigAnnotation = "mirror.openshift.io/catalog-build-sig"

// catalogBuildDigestsAnnotation records the fingerprint of the resolved
// catalog digests (see catalogDigestsFingerprint) the current catalog images
// were built from, so a change in resolved upstream content triggers a
// rebuild once it is fully mirrored.
const catalogBuildDigestsAnnotation = "mirror.openshift.io/catalog-build-digests"

// catalogRecollectSigAnnotation records the
// mirrorv1alpha1.RecollectHonoredAnnotation value that was last honored as a
// catalog rebuild trigger. The manager writes a new, unique value each time
// it honors a recollect and never removes it, so comparing against this
// record turns each recollect into exactly one rebuild — also when the build
// has to wait until the re-resolved images are mirrored.
const catalogRecollectSigAnnotation = "mirror.openshift.io/catalog-build-recollect-sig"

// catalogPollSigAnnotation records the is.Status.LastSuccessfulPollTime value
// that was last honored as a poll-expiry catalog rebuild trigger. That
// timestamp is owned by the Manager pod and can stay unchanged for a long
// time after it goes stale, so without this dedup every reconcile that finds
// the rebuilt job freshly terminal (Succeeded/Failed) would see pollExpired
// still true and force yet another rebuild — an unbounded loop of real
// builds, each pushing a fresh catalog image to the registry.
const catalogPollSigAnnotation = "mirror.openshift.io/catalog-build-poll-sig"

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=imagesets/finalizers,verbs=update
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrortargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *ImageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, rerr error) {
	l := log.FromContext(ctx)

	defer func() {
		if rerr != nil {
			ocmetrics.ReconcileErrorsTotal.WithLabelValues(req.Namespace, req.Name, "imageset").Inc()
		}
	}()

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
		setCondition(&is.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "Unbound", err.Error(), is.Generation)
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
	// A job is only ever created (or recreated) once the ImageSet has no
	// pending images left at all — see mirrorSnapshot.
	if err := r.reconcileCatalogBuildJobs(ctx, is, mt, pollExpired); err != nil {
		l.Error(err, "Failed to reconcile catalog build jobs")
		setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse, "CatalogBuildFailed", err.Error(), is.Generation)
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

	// Expose the last successful poll time as a Prometheus gauge so dashboards
	// and alerts can detect stale ImageSets without querying the API server.
	if is.Status.LastSuccessfulPollTime != nil {
		ocmetrics.ImageSetLastPollSeconds.WithLabelValues(is.Namespace, is.Name).Set(
			float64(is.Status.LastSuccessfulPollTime.Unix()),
		)
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
//
// Invariant: a CatalogBuildJob is only ever created — and an existing one
// only ever deleted to make way for a rebuild — while the ImageSet has no
// pending images left (see loadMirrorSnapshot), and the Job pulls exactly the
// catalog digest those images were resolved from. No trigger, the recollect
// annotation included, bypasses this: a catalog must never advertise a bundle
// that is not in the target registry yet.
func (r *ImageSetReconciler) reconcileCatalogBuildJobs( //nolint:gocyclo
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

	snap := loadMirrorSnapshot(ctx, r.Client, is)

	alreadyBuilt := false
	for _, c := range is.Status.Conditions {
		if c.Type == conditionCatalogReady && c.Status == metav1.ConditionTrue {
			alreadyBuilt = true
			break
		}
	}

	phases := make(map[string]builder.JobPhase, len(operators))
	anyActive := false // a job is Pending or Running
	anyJob := false    // a job exists in any phase
	for _, op := range operators {
		if op.Catalog == "" {
			continue
		}
		phase, err := builder.GetBuildJobStatus(ctx, r.Client, builder.JobName(is.Name, op.Catalog), is.Namespace)
		if err != nil {
			return err
		}
		phases[op.Catalog] = phase
		if phase != builder.JobPhaseNotFound {
			anyJob = true
		}
		if phase == builder.JobPhasePending || phase == builder.JobPhaseRunning {
			anyActive = true
		}
	}

	// Nothing built yet, no job to report on, and images still pending: wait.
	// Existing jobs keep being reported below — creating a new one, or
	// deleting one for a rebuild, is what requires nothing to be pending.
	if !snap.complete && !alreadyBuilt && !anyJob {
		return r.deferCatalogBuild(ctx, is, snap, "building")
	}

	buildSig := r.CatalogBuildMgr.BuildSignature(operators)
	digestsFP := catalogDigestsFingerprint(snap.digests, operators)
	recollectValue := is.Annotations[mirrorv1alpha1.RecollectHonoredAnnotation]
	lastSig := is.Annotations[catalogBuildSigAnnotation]
	lastDigests := is.Annotations[catalogBuildDigestsAnnotation]
	lastHandledRecollect := is.Annotations[catalogRecollectSigAnnotation]
	lastHandledPoll := is.Annotations[catalogPollSigAnnotation]

	// Each recollect the manager honored forces exactly one rebuild: the
	// marker stays set, and re-honoring it each time would keep
	// deleting+recreating the job.
	recollectForcesRebuild := recollectValue != "" && recollectValue != lastHandledRecollect
	sigChanged := lastSig != "" && lastSig != buildSig
	// The catalog content the manager resolved (and mirrored) differs from
	// what the current catalog image was built from — e.g. the upstream tag
	// moved on a poll or recollect. A catalog built before digests were
	// recorded (lastDigests == "") is rebuilt once as well.
	contentChanged := digestsFP != "" && lastDigests != digestsFP && (lastDigests != "" || alreadyBuilt)

	catalogNeedsRebuild := recollectForcesRebuild || sigChanged || contentChanged
	switch {
	case recollectForcesRebuild:
		l.Info("Catalog rebuild requested via recollect annotation", "imageSet", is.Name)
	case sigChanged:
		l.Info("Catalog build signature changed, forcing rebuild", "old", lastSig, "new", buildSig)
	case contentChanged:
		l.Info("Resolved catalog content changed, forcing rebuild", "imageSet", is.Name)
	}

	// Force a rebuild when the poll interval expired — upstream catalog images
	// may have been updated in-place. Only when no job is active (so a running
	// build is never killed), and once per LastSuccessfulPollTime value, which
	// only advances when the manager completes a fresh resolve.
	pollForcesRebuild := false
	currentPollMarker := ""
	if is.Status.LastSuccessfulPollTime != nil {
		currentPollMarker = is.Status.LastSuccessfulPollTime.Time.UTC().Format(time.RFC3339)
	}
	if pollExpired && !catalogNeedsRebuild && currentPollMarker != lastHandledPoll && !anyActive {
		l.Info("Poll interval expired, forcing catalog rebuild")
		catalogNeedsRebuild = true
		pollForcesRebuild = true
	}

	if catalogNeedsRebuild && !snap.complete {
		return r.deferCatalogBuild(ctx, is, snap, "rebuilding")
	}

	// A build is still running from the previous inputs: let it finish before
	// recording the new ones, otherwise its result would be taken for the
	// rebuild. The rebuild follows once the job is terminal.
	if catalogNeedsRebuild && anyActive {
		l.Info("Catalog rebuild waits for the running build job to finish", "imageSet", is.Name)
		setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse, "CatalogBuildRunning",
			"a catalog build job is still running; the rebuild starts once it has finished", is.Generation)
		return r.Status().Update(ctx, is)
	}

	// Record what is about to be built IMMEDIATELY, so subsequent reconciles
	// don't see the same mismatch again and endlessly delete+recreate jobs.
	if snap.complete && (catalogNeedsRebuild || lastSig != buildSig || (digestsFP != "" && lastDigests != digestsFP)) {
		if is.Annotations == nil {
			is.Annotations = make(map[string]string)
		}
		is.Annotations[catalogBuildSigAnnotation] = buildSig
		if digestsFP != "" {
			is.Annotations[catalogBuildDigestsAnnotation] = digestsFP
		}
		if recollectForcesRebuild {
			is.Annotations[catalogRecollectSigAnnotation] = recollectValue
		}
		if pollForcesRebuild {
			is.Annotations[catalogPollSigAnnotation] = currentPollMarker
		}
		if err := r.Update(ctx, is); err != nil {
			return fmt.Errorf("failed to persist catalog build signature: %w", err)
		}
	}

	// If the catalog is already built from the current inputs, don't recreate
	// jobs that were cleaned up by TTL.
	catalogAlreadyReady := alreadyBuilt && !catalogNeedsRebuild

	allSucceeded := true
	anyFailed := false

	for _, op := range operators {
		if op.Catalog == "" {
			continue
		}

		// Use the full IncludePackage slice (with channel/version filters) unless op.Full
		// is set (which means mirror everything, no package filtering).
		var packages []mirrorv1alpha1.IncludePackage
		if !op.Full {
			packages = op.Packages
		}

		targetRef := resources.CatalogTargetImage(mt.Spec.Registry, op)
		jobName := builder.JobName(is.Name, op.Catalog)
		phase := phases[op.Catalog]

		// Only ever delete a terminal (Succeeded/Failed) job for a rebuild —
		// never one that is Pending/Running.
		if catalogNeedsRebuild && (phase == builder.JobPhaseSucceeded || phase == builder.JobPhaseFailed) {
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

		if phase == builder.JobPhaseNotFound {
			if !snap.complete {
				return r.deferCatalogBuild(ctx, is, snap, "building")
			}
			pullCatalog, pinOK := pinnedCatalogRef(snap.digests, op)
			if !pinOK {
				l.Info("Catalog build deferred: no resolved digest recorded yet for catalog entry",
					"imageSet", is.Name, "catalog", op.Catalog)
				setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse,
					reasonWaitingForOperatorMirror,
					"waiting for the manager to record a resolved catalog digest before building the filtered catalog",
					is.Generation)
				return r.Status().Update(ctx, is)
			}
			if err := r.CatalogBuildMgr.EnsureCatalogBuildJob(ctx, r.Client, is, mt, op.Catalog, pullCatalog, targetRef, packages); err != nil {
				return fmt.Errorf("failed to ensure CatalogBuildJob for %s: %w", op.Catalog, err)
			}
			var err error
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
		setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse, "CatalogBuildFailed", "one or more catalog build jobs failed", is.Generation)
	case allSucceeded:
		// Use RetryOnConflict because the per-MirrorTarget manager pod is a
		// concurrent writer of ImageSet.status (TotalImages, MirroredImages,
		// etc.). Without retry, the status.Update for CatalogReady=True would
		// fail silently whenever the manager writes in the same instant.
		err := utilretry.RetryOnConflict(utilretry.DefaultRetry, func() error {
			fresh := &mirrorv1alpha1.ImageSet{}
			if rerr := r.Get(ctx, types.NamespacedName{Name: is.Name, Namespace: is.Namespace}, fresh); rerr != nil {
				return rerr
			}
			setCondition(&fresh.Status.Conditions, conditionCatalogReady, metav1.ConditionTrue,
				"CatalogBuildSucceeded", "all catalog images built successfully", fresh.Generation)
			return r.Status().Update(ctx, fresh)
		})
		return err
	default:
		setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse, "CatalogBuildRunning", "catalog build jobs are still running", is.Generation)
	}

	return r.Status().Update(ctx, is)
}

// deferCatalogBuild records that a catalog (re)build is waiting for the
// ImageSet's mirroring to finish.
func (r *ImageSetReconciler) deferCatalogBuild(ctx context.Context, is *mirrorv1alpha1.ImageSet, snap mirrorSnapshot, verb string) error {
	l := log.FromContext(ctx)
	var msg string
	switch {
	case !snap.knowState:
		l.Info("Catalog build deferred: imagestate not yet populated by manager", "imageSet", is.Name)
		msg = fmt.Sprintf("waiting for the manager to resolve and mirror the ImageSet before %s the filtered catalog", verb)
	case snap.pending > 0:
		l.Info("Catalog build deferred: images still mirroring", "imageSet", is.Name, "pending", snap.pending)
		msg = fmt.Sprintf("waiting for all images of the ImageSet to be mirrored (%d pending) before %s the filtered catalog", snap.pending, verb)
	default:
		l.Info("Catalog build deferred: current spec not fully resolved yet", "imageSet", is.Name)
		msg = fmt.Sprintf("waiting for the manager to resolve the current spec before %s the filtered catalog", verb)
	}
	setCondition(&is.Status.Conditions, conditionCatalogReady, metav1.ConditionFalse,
		reasonWaitingForOperatorMirror, msg, is.Generation)
	return r.Status().Update(ctx, is)
}

// mirrorSnapshot is one consistent read of an ImageSet's imagestate ConfigMap.
type mirrorSnapshot struct {
	// complete is true when the ImageSet has no pending images at all: every
	// entry, of any origin, is Mirrored or PermanentlyFailed — and the state
	// reflects the current spec (see loadMirrorSnapshot).
	complete bool
	// knowState is false when the manager has not written any state yet.
	knowState bool
	// pending counts entries that are neither Mirrored nor PermanentlyFailed.
	pending int
	// digests are the catalog-digest annotations the entries were resolved
	// from (imagestate.CatalogDigestsAnnotation), written in the same
	// ConfigMap update as the entries themselves.
	digests map[string]string
}

// loadMirrorSnapshot reads the ImageSet's imagestate ConfigMap and reports
// whether a catalog may be built from it.
func loadMirrorSnapshot(ctx context.Context, c client.Client, is *mirrorv1alpha1.ImageSet) mirrorSnapshot {
	state, digests, err := imagestate.LoadWithCatalogDigests(ctx, c, is.Namespace, is.Name)
	if err != nil || len(state) == 0 {
		return mirrorSnapshot{}
	}
	snap := mirrorSnapshot{knowState: true, digests: digests}

	// Expected per-entry signatures of the CURRENT spec: after an operator is
	// added or changed, the state still shows the previously resolved
	// content until the manager has resolved the new entry.
	expectedSigs := make(map[string]bool, len(is.Spec.Mirror.Operators))
	for _, op := range is.Spec.Mirror.Operators {
		if op.Catalog != "" {
			expectedSigs[mirrorv1alpha1.OperatorEntrySignature(op)] = false
		}
	}

	hasOperator := false
	legacySigSeen := false
	for _, e := range state {
		if e == nil {
			continue
		}
		if e.State != "Mirrored" && !e.PermanentlyFailed {
			snap.pending++
		}
		if e.Origin != imagestate.OriginOperator {
			continue
		}
		hasOperator = true
		if e.EntrySig == "" {
			// Written before per-entry signatures existed — cannot be
			// attributed to a specific spec entry.
			legacySigSeen = true
		} else if _, ok := expectedSigs[e.EntrySig]; ok {
			expectedSigs[e.EntrySig] = true
		}
	}

	if snap.pending > 0 || !hasOperator {
		return snap
	}
	// The manager only advances ObservedGeneration once it has cleanly
	// resolved the whole current spec; until then the state may be missing
	// entries a spec change should have added.
	if is.Status.ObservedGeneration != is.Generation {
		return snap
	}
	if !legacySigSeen {
		for _, seen := range expectedSigs {
			if !seen {
				return snap
			}
		}
	}
	snap.complete = true
	return snap
}

// catalogDigestsFingerprint identifies the resolved content of every
// configured catalog, or "" when any catalog has no recorded digest yet.
func catalogDigestsFingerprint(digests map[string]string, operators []mirrorv1alpha1.Operator) string {
	parts := make([]string, 0, len(operators))
	for _, op := range operators {
		if op.Catalog == "" {
			continue
		}
		key := mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))
		digest, ok := mirrorv1alpha1.ParseOperatorCacheDigest(digests[key])
		if !ok {
			return ""
		}
		parts = append(parts, key+"="+digest)
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// pinnedCatalogRef returns op.Catalog rewritten to reference the exact
// catalog digest the ImageSet's imagestate entries were resolved from
// (imagestate.CatalogDigestsAnnotation, see mirrorSnapshot.digests). Returns
// ok=false if no parseable digest is recorded for op.
//
// op.Catalog is normally a mutable tag. Pulling it unpinned would let the Job
// land on newer upstream content than the manager resolved — and pinning to
// the ImageSet's own cache annotation is not enough either: the manager
// updates that annotation as soon as it resolves a new digest, before the new
// digest's Pending entries are written to the imagestate ConfigMap, so a
// build started in between would see "everything Mirrored" for content that
// has not been mirrored at all. The digest stored with the entries has no
// such window.
func pinnedCatalogRef(digests map[string]string, op mirrorv1alpha1.Operator) (string, bool) {
	annoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))
	digest, ok := mirrorv1alpha1.ParseOperatorCacheDigest(digests[annoKey])
	if !ok {
		return "", false
	}
	pinned, err := catalog.PinDigest(op.Catalog, digest)
	if err != nil {
		return "", false
	}
	return pinned, true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		// Watch the imagestate ConfigMaps ("<imageset>-images", plus the
		// legacy per-MirrorTarget "<mirrortarget>-images") written by the
		// manager pod, so the catalog-build gate is re-evaluated as soon as the
		// last pending image is mirrored instead of at the next pollInterval.
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.imageStateConfigMapToRequests),
		).
		Complete(r)
}

// imageStateConfigMapToRequests maps an imagestate ConfigMap to the
// ImageSet(s) whose catalog-build gate it feeds: "<imageset>-images" to that
// ImageSet, the legacy "<mirrortarget>-images" to every ImageSet of the target.
func (r *ImageSetReconciler) imageStateConfigMapToRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetName()
	const suffix = "-images"
	if !strings.HasSuffix(name, suffix) {
		return nil
	}
	owner := strings.TrimSuffix(name, suffix)
	is := &mirrorv1alpha1.ImageSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner}, is); err == nil {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: owner, Namespace: obj.GetNamespace()}}}
	}
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner}, mt); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, isName := range mt.Spec.ImageSets {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      isName,
				Namespace: obj.GetNamespace(),
			},
		})
	}
	return requests
}
