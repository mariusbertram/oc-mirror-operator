package manager

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
	"github.com/regclient/regclient/types/ref"
)

// Architecture reconcile
//
// An image is mirrored with the architecture selection of its owning
// ImageSets (spec.mirror.architectures, see ownerArchitectures) and the
// selection is recorded in ImageEntry.Architectures. When the selection
// changes later, Mirrored images are reconciled by a worker without leaving
// Mirrored — IDMS/ITMS and the catalog-build gate only consider Mirrored
// entries, so resetting them to Pending would withdraw their mirror mappings
// until the reconcile finished:
//
//   - backfill: the selection gained an architecture the image was not
//     mirrored with; the worker copies the missing children (the manifest
//     list itself is already there and unchanged).
//   - prune: the selection lost an architecture and the MirrorTarget has
//     cleanup-policy=Delete; the worker deletes the children outside the
//     retain set of the image's repository (see archRetainByRepo).
//
// Either way the manifest list digest stays the same. A failed reconcile is
// retried with backoff (ArchRetryCount/ArchNextRetryAt) and never touches
// State, LastError or RetryCount.

// archUnion accumulates the architectures images of one repository need;
// all is set once any of them needs every architecture.
type archUnion struct {
	all   bool
	archs map[string]bool
}

func (u *archUnion) add(archs []string) {
	if len(archs) == 0 {
		u.all = true
		return
	}
	if u.archs == nil {
		u.archs = map[string]bool{}
	}
	for _, a := range archs {
		u.archs[a] = true
	}
}

// list returns the union, or nil when some image needs every architecture.
func (u *archUnion) list() []string {
	if u == nil || u.all || len(u.archs) == 0 {
		return nil
	}
	out := make([]string, 0, len(u.archs))
	for a := range u.archs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// repoOf returns the registry/repository of an image reference, or the
// reference itself when it cannot be parsed.
func repoOf(image string) string {
	r, err := ref.New(image)
	if err != nil {
		return image
	}
	return r.Registry + "/" + r.Repository
}

// archSelections computes, for every entry with owners, the architectures it
// should be mirrored with (nil = all), and per repository the architectures
// that may not be pruned from any image in it: the union of the selections of
// all its images, nil when one of them needs every architecture. Child
// manifests are per repository, and two manifest lists in one repository can
// share a child, so an architecture is only pruned when no image of the
// repository needs it anymore.
func (m *MirrorManager) archSelectionsLocked(archByIS map[string][]string) (wantByDest map[string][]string, retainByRepo map[string][]string) {
	wantByDest = make(map[string][]string, len(m.imageState))
	unions := map[string]*archUnion{}
	for dest, entry := range m.imageState {
		owners := m.owners[dest]
		if len(owners) == 0 || entry == nil || mirror.IsChartSource(entry.Source) {
			continue
		}
		want := ownerArchitectures(owners, archByIS)
		wantByDest[dest] = want
		repo := repoOf(dest)
		u := unions[repo]
		if u == nil {
			u = &archUnion{}
			unions[repo] = u
		}
		u.add(want)
	}
	retainByRepo = make(map[string][]string, len(unions))
	for repo, u := range unions {
		retainByRepo[repo] = u.list()
	}
	return wantByDest, retainByRepo
}

// covers reports whether an image holding the architectures have (nil = all)
// holds every architecture of need (nil = all).
func covers(have, need []string) bool {
	if have == nil {
		return true
	}
	if need == nil {
		return false
	}
	for _, a := range need {
		if !slices.Contains(have, a) {
			return false
		}
	}
	return true
}

// archReconcileNeeded decides what a Mirrored image recorded with the
// architectures recorded (what its destination may still hold) needs now that
// its selection is want: backfill when want has an architecture beyond
// recorded; prune (only with pruneEnabled and a retain set for the
// repository) when recorded has one the repository no longer needs. An
// architecture another image of the repository still needs stays recorded
// after a prune, so it is pruned once that need goes away.
func archReconcileNeeded(recorded, want []string, pruneEnabled bool, retain []string) (backfill, prune bool) {
	backfill = !covers(recorded, want)
	prune = pruneEnabled && want != nil && len(retain) > 0 && !covers(retain, recorded)
	return backfill, prune
}

// unionArchs returns a ∪ b for architecture selections (nil = all).
func unionArchs(a, b []string) []string {
	if a == nil || b == nil {
		return nil
	}
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// intersectArchs returns a ∩ b for architecture selections (nil = all).
func intersectArchs(a, b []string) []string {
	if a == nil {
		return slices.Clone(b)
	}
	if b == nil {
		return slices.Clone(a)
	}
	out := []string{}
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// archReconcileItemLocked returns the batch item reconciling the Mirrored
// image dest, if its architectures need reconciling and its backoff passed.
func (m *MirrorManager) archReconcileItemLocked(dest string, entry *imagestate.ImageEntry, want []string, skipMissing, pruneEnabled bool, retainByRepo map[string][]string, now time.Time) (BatchItem, bool) {
	// Helm charts are single artifacts without architectures, and the
	// worker's chart path does not report a reconcile.
	if m.inProgress[dest] != "" || mirror.IsChartSource(entry.Source) {
		return BatchItem{}, false
	}
	retain := retainByRepo[repoOf(dest)]
	backfill, prune := archReconcileNeeded(entry.Architectures, want, pruneEnabled, retain)
	if !backfill && !prune {
		// Nothing to reconcile (anymore): drop a backoff left by a failed
		// attempt for a selection that has since changed.
		if entry.ArchRetryCount != 0 || entry.ArchNextRetryAt != nil {
			entry.ArchRetryCount, entry.ArchNextRetryAt = 0, nil
			m.stateDirty = true
		}
		return BatchItem{}, false
	}
	if entry.ArchNextRetryAt != nil && now.Before(entry.ArchNextRetryAt.Time) {
		return BatchItem{}, false
	}
	item := BatchItem{
		Source:                   entry.Source,
		Dest:                     dest,
		Architectures:            want,
		SkipMissingArchitectures: skipMissing,
		Backfill:                 backfill,
	}
	if prune {
		item.PruneRetain = retain
	}
	oclog.Printf("Reconciling architectures of %s: mirrored with %v, selected %v (backfill=%v, prune=%v)\n",
		dest, archList(entry.Architectures), archList(want), backfill, prune)
	return item, true
}

// handleConfirmPrune answers a worker's confirmation, right before deleting,
// that removing dest's children outside retain is still allowed. It must
// still hold for the plan the manager last computed — dest Mirrored, the
// cleanup policy Delete, and retain covering every architecture dest's
// repository needs — and for the live cluster: the MirrorTarget must still
// carry cleanup-policy=Delete and reference the same ImageSets, each at the
// generation the plan was computed from. The cached plan alone is not enough:
// it is only refreshed after image resolution, which can take long, so a
// revoked policy or a changed selection would otherwise go unnoticed. It
// fails closed — 410 when the plan is stale, 503 when the state cannot be
// read — because the deletion cannot be undone.
func (m *MirrorManager) handleConfirmPrune(w http.ResponseWriter, r *http.Request, dest string, retain []string) {
	if !m.tryRLockFor(shouldMirrorLockWait) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	entry, ok := m.imageState[dest]
	mirrored := ok && entry.State == stateMirrored
	current, known := m.retainArchByRepo[repoOf(dest)]
	prune := m.pruneArchitectures
	generations := maps.Clone(m.archPlanGenerations)
	m.mu.RUnlock()

	if !mirrored || !prune || !known || current == nil || !covers(retain, current) {
		oclog.Printf("Refusing prune of %s: plan keeps %v, repository now needs %v (cleanup-policy Delete: %v)\n",
			dest, retain, archList(current), prune)
		w.WriteHeader(http.StatusGone)
		return
	}
	if m.Client == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), shouldMirrorLockWait)
	defer cancel()
	if stale, err := m.prunePlanStale(ctx, generations); err != nil {
		oclog.Printf("Cannot confirm prune of %s: %v\n", dest, err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	} else if stale != "" {
		oclog.Printf("Refusing prune of %s: %s\n", dest, stale)
		w.WriteHeader(http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// prunePlanStale checks the live MirrorTarget and ImageSets against the
// state a prune plan was computed from and says why the plan is stale, or ""
// if it still holds.
func (m *MirrorManager) prunePlanStale(ctx context.Context, generations map[string]int64) (string, error) {
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt); err != nil {
		return "", fmt.Errorf("get MirrorTarget: %w", err)
	}
	if mt.Annotations[mirrorv1alpha1.CleanupPolicyAnnotation] != mirrorv1alpha1.CleanupPolicyDelete {
		return "cleanup-policy is no longer Delete", nil
	}
	if len(mt.Spec.ImageSets) != len(generations) {
		return "the MirrorTarget's ImageSets changed", nil
	}
	for _, name := range mt.Spec.ImageSets {
		planned, ok := generations[name]
		if !ok {
			return "ImageSet " + name + " was added", nil
		}
		is := &mirrorv1alpha1.ImageSet{}
		if err := m.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: m.Namespace}, is); err != nil {
			if apierrors.IsNotFound(err) {
				return "ImageSet " + name + " was deleted", nil
			}
			return "", fmt.Errorf("get ImageSet %s: %w", name, err)
		}
		if is.Generation != planned {
			return fmt.Sprintf("ImageSet %s changed (generation %d, planned with %d)", name, is.Generation, planned), nil
		}
	}
	return "", nil
}

// archList renders an architecture selection for logs.
func archList(archs []string) any {
	if len(archs) == 0 {
		return "all"
	}
	return archs
}

// applyReconcileStatusLocked applies a worker's architecture reconcile
// result. The entry's State is never changed. Caller must hold m.mu.
func (m *MirrorManager) applyReconcileStatusLocked(req WorkerStatusRequest) {
	entry, ok := m.imageState[req.Destination]
	if !ok {
		return
	}
	if req.Error != "" {
		oclog.Printf("Architecture reconcile of %s failed (image stays mirrored): %s\n", req.Destination, req.Error)
		m.archBackoffLocked(entry)
		return
	}
	// What the destination may still hold: the new selection, plus what it
	// held before — minus, after a prune, what the prune removed.
	kept := entry.Architectures
	if req.PruneRetain != nil {
		kept = intersectArchs(entry.Architectures, req.PruneRetain)
	}
	m.recordArchitecturesLocked(entry, unionArchs(req.Architectures, kept))
}

// recordArchitecturesLocked records the architecture selection entry was
// mirrored with and clears any reconcile backoff. Caller must hold m.mu.
func (m *MirrorManager) recordArchitecturesLocked(entry *imagestate.ImageEntry, archs []string) {
	var recorded []string
	if len(archs) > 0 {
		recorded = slices.Clone(archs)
		sort.Strings(recorded)
	}
	if slices.Equal(entry.Architectures, recorded) && entry.ArchRetryCount == 0 && entry.ArchNextRetryAt == nil {
		return
	}
	entry.Architectures = recorded
	entry.ArchRetryCount = 0
	entry.ArchNextRetryAt = nil
	m.stateDirty = true
}

// copyArchState carries the architecture bookkeeping of from over to a
// rebuilt entry for the same mirrored image.
func copyArchState(to, from *imagestate.ImageEntry) {
	to.Architectures = slices.Clone(from.Architectures)
	to.ArchRetryCount = from.ArchRetryCount
	to.ArchNextRetryAt = from.ArchNextRetryAt
}

// archBackoffLocked schedules the next architecture reconcile attempt of
// entry after a failure. Caller must hold m.mu.
func (m *MirrorManager) archBackoffLocked(entry *imagestate.ImageEntry) {
	entry.ArchRetryCount++
	next := metav1.NewTime(time.Now().Add(retryBackoff(entry.ArchRetryCount)))
	entry.ArchNextRetryAt = &next
	m.stateDirty = true
}
