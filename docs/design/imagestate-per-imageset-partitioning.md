# Per-ImageSet State Partitioning + Shared-Image Index — Design Proposal

Status: Proposed
Issue: #104

## 1. Problem

`pkg/mirror/imagestate/` stores all per-image mirroring state for a
MirrorTarget in a single consolidated ConfigMap (`<mt>-images`,
`ConfigMapNameForTarget`). Every entry carries a `Refs []ImageRef` listing
which ImageSet(s) require that destination image (`imagestate.go:47-85`).

This consolidated, single-blob design has two consequences that #104 calls out:

1. **Every check touches every ImageSet's images.** A spec edit or
   `mirror.openshift.io/recollect` on one ImageSet causes the manager to
   re-resolve and the controllers to re-derive gate/count status against the
   *entire* target's state (`LoadForTarget` loads the whole map;
   `manager_resolve.go`'s `filterByImageSet` then filters it down in memory
   per-ImageSet on every pass). With N ImageSets on a target, work scales with
   the whole target instead of with the one ImageSet that changed, and nothing
   about the per-ImageSet portion of that work is parallelizable — it is all
   one in-memory map guarded by one CM read/write.
2. **Cleanup safety already exists, but only by scanning everything.**
   `partitionAndCreateCleanupJob` (`mirrortarget_controller.go:851-911`) and
   `reconcileOrphans` (`mirrortarget_controller.go:729-766`) already do the
   right thing — an image is only queued for deletion when `len(entry.Refs) ==
   1` (or `0` for orphans), i.e. no other ImageSet still needs it. That
   correctness comes at the cost of loading and holding the full consolidated
   map for every cleanup decision, even when only one small ImageSet is being
   torn down out of fifteen.

Both are the same root cause: **ImageSet-scoped operations (check, resolve,
cleanup) are implemented against a MirrorTarget-scoped data structure.**

This proposal is about *where the data lives*, not how it is encoded. It is
complementary to, not a replacement for, the schema-v2/sharding work in
`docs/design/imagestate-scaling.md` (PR #101) — see §6.

## 2. Goals

- An ImageSet's state check/update reads and writes only that ImageSet's own
  state, sized to that ImageSet, never the whole MirrorTarget.
- Determining "is this image still needed elsewhere" for cleanup stays exact
  (no false deletes across ImageSets) but no longer requires loading every
  other ImageSet's full state.
- Per-ImageSet checks become independently parallelizable, the same way
  worker pods already mirror image batches independently (issue #104's "the
  checks could work in a way as the mirror worker" ask).

## 3. Proposed architecture

### 3.1 Per-ImageSet state ConfigMaps

Replace the single `<mt>-images` blob with one ConfigMap per ImageSet:

```
<imageset>-images   →  dest → { source, state, retryCount, lastError,
                                 permanentlyFailed, origin, entrySig, originRef }
```

Because the CM is already scoped to one ImageSet, entries drop the `Refs`
slice entirely — there is nothing to disambiguate within a single ImageSet's
own view. This is close to the *pre-consolidation* per-ImageSet CM
(`ConfigMapName`, still kept today only for legacy/deprecated paths) — the
difference is it becomes the primary store again, deliberately, with the
sharing problem solved by §3.2 instead of by folding everything into one map.

The manager resolves and updates exactly one ImageSet's CM per resolve pass.
Gate checks and counts (`imageset_controller.go`) read exactly that one CM —
O(ImageSet), not O(MirrorTarget).

### 3.2 Shared-image index

A single small ConfigMap per MirrorTarget, `<mt>-images-index`, tracks only
images referenced by **more than one** ImageSet:

```
<mt>-images-index   →  dest → [imageSetName, imageSetName, ...]
```

Images used by exactly one ImageSet never appear here — in real deployments
(operator catalogs, release payloads) the large majority of images are
exclusive to the ImageSet that resolved them, so this index stays small
compared to the full image set, independent of catalog size.

The manager updates the index as a side effect of merging its resolve result
for one ImageSet:

- An image gains a reference: if the index has no entry for that dest, do
  nothing yet — a dest only enters the index the moment a *second* ImageSet
  references it. On the second reference, create the index entry with both
  ImageSet names.
- An image loses a reference (narrowed spec, blocked, or ImageSet removed):
  remove that ImageSet's name from the index entry. If only one name remains,
  delete the index entry entirely (it reverts to being exclusive, tracked
  solely in that one ImageSet's own CM). If zero names remain (shouldn't
  happen — the entry is removed at one-remaining, not zero — kept as an
  invariant check).

This makes the index a pure *overflow table* for the shared case, not a
mirror of every entry's ownership.

### 3.3 Cleanup

On ImageSet removal (`reconcileRemovedImageSets`):

1. Load only the removed ImageSet's own CM (`<imageset>-images`) — this is
   the full candidate list, already scoped, no filtering needed.
2. Load the (small) shared-image index for the MirrorTarget.
3. Partition: any dest present in the index is shared → skip deletion, just
   remove this ImageSet's name from the index entry (delete the entry if one
   name remains). Any dest *not* present in the index is exclusive → queue
   for the cleanup Job, same as today's `exclusiveState` snapshot.
4. Delete the ImageSet's own CM once the cleanup Job is created (its content
   is now fully accounted for: either snapshotted for deletion or was never
   exclusive).

This is the same exactness guarantee as today's `len(Refs) == 1` check, but
step 2 loads one small index CM instead of the full consolidated state, and
step 1 never touches any other ImageSet's data at all.

Spec-narrowing within an ImageSet (fewer packages/channels resolved) is
handled the same way it is today, just scoped: the manager's merge step for
that one ImageSet removes the dropped dests from its own CM and updates the
index (§3.2). If a dropped dest was exclusive to this ImageSet (not in the
index), it becomes an orphan candidate for the existing per-target "orphans"
cleanup path — that path is already cheap (bounded by however many images
were just dropped, not by target size) and needs no change beyond reading
from per-ImageSet CMs instead of the consolidated one.

### 3.4 Parallel checking

Because state is now physically partitioned per ImageSet, resolving/checking
N ImageSets no longer requires serializing through one shared in-memory map
and one CM. The manager can fan out per-ImageSet resolve+check passes
(bounded by a concurrency limit, mirroring how worker pods already mirror
image batches concurrently) — the only shared coordination point left is
writes to the small shared-image index, which is naturally low-traffic and
can go through the existing `CreateOrUpdate` optimistic-concurrency retry.

## 4. Consistency

Two ConfigMaps (an ImageSet's own state and the shared index) can no longer
be updated atomically. This is an explicit, accepted tradeoff, matching the
existing "multi-CM writes are not atomic" invariant already adopted for
sharding in PR #101 (§4.A of that design):

- Write order on gaining a shared reference: write the index entry *before*
  the second ImageSet's own CM is saved with that dest present. A crash
  between the two leaves the index listing an ImageSet that doesn't
  (yet) have the entry in its own CM — self-healing, because the next resolve
  pass for that ImageSet reconciles its own CM against its spec regardless.
- Write order on losing a reference (cleanup/narrowing): remove the dest from
  the losing ImageSet's own CM *before* updating the index. A crash between
  the two leaves the index still listing the ImageSet that already dropped
  the image — harmless, it only makes a future cleanup pass overly cautious
  (treats the image as shared one cycle longer than necessary) rather than
  causing an incorrect delete.

Both orderings bias toward "never wrongly delete a shared image," matching
the existing safety property of the Refs-based design.

## 5. Migration

- On first reconcile after upgrade, split the existing consolidated
  `<mt>-images` CM into per-ImageSet CMs plus the shared-image index (an
  entry appears in the index iff it currently has `len(Refs) > 1`), then
  delete the consolidated CM.
- Keep a read fallback: if an ImageSet's own CM does not exist yet but the
  legacy consolidated CM does, derive it via `filterByImageSet` once,
  matching the existing v1-compat pattern in `imagestate.go`.
- `ConfigMapName(imageSetName)` (currently `// Deprecated`, kept only for
  legacy per-ImageSet CM reads in `cmd/main.go`/`cmd/worker/main.go`) becomes
  the primary name again; the deprecation comment and the now-unused
  `ConfigMapNameForTarget` path are removed once migration is complete.

## 6. Relationship to PR #101 (schema v2 / sharding)

PR #101 shards the *existing consolidated* blob by hash bucket to stay under
the 1 MiB ConfigMap limit and cut write amplification, while keeping one
logical per-target state with `Refs`-based ownership. This proposal instead
splits the state by *ImageSet identity* to fix check-scope and cleanup-lookup
cost; size stops being the primary concern here because each per-ImageSet CM
is already bounded by one ImageSet's own image count instead of the whole
target's.

The two are complementary, not competing:

- Once state is split per-ImageSet (this proposal), an individual ImageSet's
  CM can still hit the 1 MiB ceiling for a single very large catalog — PR
  #101's schema-v2 compact encoding applies per-ImageSet CM exactly as it
  would per-shard today, just with the hash-bucket sharding layer no longer
  needed (each ImageSet's own CM already plays the role of a shard).
- PR #101's Stage 2/3 (manager-served summary + PVC tier) remains the answer
  for deployments that outgrow ConfigMaps altogether regardless of how the
  data is partitioned.

Recommended sequencing: land this proposal's per-ImageSet split first (it is
the smaller, more contained change and directly closes #104), then land PR
#101's schema-v2 encoding on top, scoped per-ImageSet CM instead of per-target
shard.

## 7. Impact map

| Area | Change |
|---|---|
| `pkg/mirror/imagestate/` | `ImageEntry.Refs` removed (scope is now implicit); add index CM encode/decode; per-ImageSet `Load/Save` becomes primary API; `LoadForTarget`/`SaveForTarget` removed after migration |
| `pkg/mirror/manager/manager_resolve.go` | merge step writes one ImageSet's own CM + updates the shared index instead of merging into one consolidated map; `filterByImageSet` no longer needed (each CM is already scoped) |
| `pkg/mirror/manager/manager.go` | per-ImageSet resolve/check passes can run concurrently (bounded pool), matching worker mirroring concurrency |
| `internal/controller/mirrortarget_controller.go` | `reconcileCleanup`/`reconcileOrphans`/`partitionAndCreateCleanupJob` read the removed ImageSet's own CM + the index instead of the consolidated map |
| `internal/controller/imageset_controller.go` | gate check reads one ImageSet's own CM (no filtering step) |
| ConfigMap watch mapping | `imagestate.TargetNameFromStateCM` extended to also map `<mt>-images-index` and per-ImageSet CM names back to the MirrorTarget |
| RBAC | none (same ConfigMap verbs; more CM names, not new permissions) |
| `pkg/resourceapi/server.go` | reads become per-ImageSet CM + index instead of one consolidated CM |
