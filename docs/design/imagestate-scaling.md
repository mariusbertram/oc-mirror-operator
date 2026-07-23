# ImageState Store Scaling — Design Proposal

Status: proposal
Issue: consolidated imagestate ConfigMap reaches ~75 % of the 1 MiB limit with
15 ImageSets × 3 large Red Hat / IBM catalogs.

## 1. Problem

All per-image mirroring state for a MirrorTarget lives in a single
gzip-compressed ConfigMap (`<mt>-images`, `pkg/mirror/imagestate/`). ConfigMaps
are capped at 1 MiB. A realistic enterprise deployment (15 ImageSets, each with
3 large operator catalogs → roughly 10–15 k images) already consumes ~768 KiB.
One more catalog, a burst of `lastError` strings from a registry outage, or a
new OCP minor in the release channels pushes it over the limit — at which point
`SaveForTarget` fails, the manager cannot persist progress, and mirroring
effectively stalls.

Independent of the hard ceiling, the current design is expensive well below it:

- **Write amplification.** The manager flushes the *entire* blob on every
  reconcile tick where anything changed (`stateDirty`, manager.go Phase F).
  A single image flipping `Pending → Mirrored` rewrites ~768 KiB into etcd.
  During an active mirror run this happens continuously.
- **Watch and cache fan-out.** The operator controllers read the ConfigMap
  through the controller-runtime cached client
  (`imageset_controller.go:operatorImagesMirrored`,
  `mirrortarget_controller.go:reconcileCleanup`). The first cached `Get` starts
  an informer, so every flush ships the full object to the operator and keeps a
  decompressed copy resident in its cache. The dashboard/resource API
  (`pkg/resourceapi/server.go`) loads and decompresses the blob per request.
- **etcd pressure.** Frequent large-value updates inflate the etcd WAL and
  compaction load on the management cluster.

## 2. Why the blob is as big as it is

Per `ImageEntry` (map key = destination ref):

| Component | Typical size | Compressible? |
|---|---|---|
| dest ref (map key) incl. `@sha256:<64 hex>` | 100–150 B | digest: no (~32 B entropy) |
| `source` ref, usually same digest | 100–150 B | repo prefix: yes; digest deduped by gzip |
| `state`, `retryCount`, `permanentlyFailed` | ~40 B | yes |
| `refs[]` — per referencing ImageSet: name + `origin` + `entrySig` (64-hex hash, `api/v1alpha1/resolution_sig.go`) + `originRef` free text (`"<catalog ref> [pkg1, pkg2]"`) | 150–250 B × refs | entrySig: no |
| `lastError` (failed images) | 0–500 B | partly |

Two structural observations:

1. **Massive tuple duplication.** All images produced by the same spec entry
   carry an *identical* `(imageSet, origin, entrySig, originRef)` tuple. A
   1 000-image catalog repeats the same ~200 B tuple — half of it an
   incompressible hash — 1 000 times. gzip only recovers this when identical
   entries fall within its 32 KiB window; with 45 interleaved catalogs that is
   hit-or-miss.
2. **The digest is stored twice per image** (in source and dest); gzip
   back-references usually catch this, but the repo paths around it are still
   duplicated.

## 3. What the data actually is

This drives the whole design:

- The imagestate is a **cache of "what exists in the target registry" plus
  retry bookkeeping** (`retryCount`, `lastError`, `permanentlyFailed`) **plus
  provenance** (`refs`: which spec entry required the image).
- It is **fully reconstructible**: resolution is deterministic from the
  ImageSet spec + upstream digests, and the manager's drift-check
  (`CheckExist`, manager.go Phase D) already re-derives `Mirrored` from the
  registry itself. Losing the store costs one re-resolve plus a HEAD sweep —
  it is not durable data of record.
- At steady state there is a **single logical writer**: the manager pod. The
  MirrorTarget controller only writes during cleanup partitioning
  (`reconcileCleanup` / `SaveRaw` snapshots), and the two already have to
  coordinate today.
- Consumers never need the full state at once:
  - ImageSet controller: per-IS *aggregate* ("are all operator images of the
    current spec done?") for the catalog-build gate.
  - MirrorTarget controller: counts + cleanup *partitions* (exclusive images
    of a removed ImageSet).
  - Dashboard/resource API: paged, filtered views.
  - Cleanup job: a worklist of destinations to delete.

Conclusion: this is **operational working state of the manager**, not
control-plane configuration. etcd is the wrong home for the full-resolution
copy; only bounded aggregates belong in the Kubernetes API.

## 4. Options considered

### A. Shard across N ConfigMaps

Partition by hash of the destination into `<mt>-images-<NN>` plus a small meta
CM (shard count, schema version). Save tracks dirty shards and only rewrites
those.

- ✅ Small change, hidden entirely behind `imagestate.Load/Save`.
- ✅ Removes the imminent ceiling; dirty-shard flush cuts write amplification
  roughly by the shard count.
- ❌ Still etcd-resident; informer cache still holds everything.
- ❌ Multi-CM writes are not atomic. Tolerable — entries are independent and
  every consumer already handles partially-stale state — but it must be a
  conscious invariant.

### B. Schema v2 — normalize before compressing

Attack §2 directly with an interned encoding:

```jsonc
{
  "v": 2,
  "groups": [   // one per (imageSet, origin, entrySig, originRef) tuple
    {"is": "ibm-catalogs", "o": "operator", "sig": "ab12…", "ref": "icr.io/cpopen/ibm-operator-catalog [pkg1, pkg2]"}
  ],
  "images": {
    // dest suffix under a repo dictionary; source stored only when it does not
    // follow the standard mapping rule; refs are group indices
    "openshift4/ose-kube-rbac-proxy@sha256:…": {"s": 1, "g": [0, 7], "rc": 3}
  }
}
```

Key moves: group table for refs (the dominant duplication), repo-prefix
dictionary, derive `dest` from `source` + mapping rule instead of storing both
(store an explicit source only for exceptions), cap `lastError` length, keep
gzip on top. Estimated 5–10× reduction → the same deployment lands at
~80–150 KiB.

- ✅ No topology change; all consumers keep working through the package API.
- ✅ Also shrinks the cleanup snapshot CMs (same encoder via `SaveRaw`).
- ❌ Still a ceiling (just further away) and still full-blob rewrites.

### C. Manager-owned store, API-served (architectural fix)

The manager pod becomes the sole owner of full-resolution state:

- **Store:** embedded key-value store (bbolt) on a small PVC mounted into the
  manager Deployment (single pod, RWO is fine). Per-image writes become O(entry)
  instead of O(state). Because the state is reconstructible (§3), losing the
  PVC is a cold-start, not data loss — `emptyDir` + rebuild is even acceptable
  as a degraded mode where no StorageClass exists.
- **Reads:** the manager already runs an HTTP server consumed by workers and
  the dashboard (`/should-mirror`, resource endpoints in `pkg/resourceapi`).
  Add read endpoints: `/state/summary?imageset=…` (counts + per-sig resolution
  status for the catalog-build gate), `/state/images?filter=…&page=…` (UI),
  `/state/partition?imageset=…` (cleanup worklists).
- **Kubernetes API keeps only aggregates:** ImageSet/MirrorTarget status get
  counts, per-spec-entry gate signatures, and a *capped* failed-image list —
  all O(spec), not O(images). The controllers stop reading the big CM entirely.
- **Availability:** so the controllers can make progress while the manager pod
  is down, the manager periodically checkpoints a *small* summary CM
  (`<mt>-state-summary`: counts + gate sigs, a few KiB). Controllers prefer
  the live API and fall back to the last checkpoint.
- **Cleanup:** on ImageSet removal the controller asks the manager for the
  exclusive-image partition and passes the worklist to the Job (small CM below
  the limit thanks to schema v2, or the Job queries the manager directly). On
  MirrorTarget deletion the finalizer already serializes cleanup before the
  manager Deployment is torn down.

- ✅ etcd leaves the per-image data path entirely; no size ceiling; watch and
  cache costs collapse to the few-KiB summary.
- ✅ Matches the ownership that already exists de facto (manager = writer).
- ❌ Larger change: RBAC for PVC, HTTP surface + auth between operator and
  manager, controller fallback logic.
- ❌ Optional PVC dependency (mitigated by the rebuild property).

### D. State as OCI artifact in the target registry

Store the state blob as an OCI artifact next to the mirrored content.
Attractive properties (state survives cluster reinstall, natural fit for
future disk-to-mirror/export flows — `pkg/mirror/export` already produces
manifests), but it couples every state write to registry availability and to
artifact-type support of the target registry. Better suited as an optional
**export/backup format** on top of C than as the primary store.

## 5. Recommendation — two stages

**Stage 1 (next release, low risk): B + A behind the existing package API.**
Schema v2 interned encoding, plus hash-sharding with dirty-shard flush as the
safety valve. Everything stays inside `pkg/mirror/imagestate/`; no consumer
outside the package changes. This buys an order of magnitude of headroom and
cuts etcd churn immediately.

**Stage 2 (architectural): C.** Promote the manager to sole state owner with a
bbolt/PVC store and HTTP read endpoints; demote the ConfigMap to a small
summary checkpoint; move controllers and dashboard to the API with checkpoint
fallback; switch cleanup worklists to manager-served partitions. D remains an
optional export format later.

Stage 1 is worth doing even if Stage 2 follows soon: the encoder is reused for
cleanup snapshots and the summary checkpoint, and it de-risks the interim.

## 6. Migration

- Add `schemaVersion` to the CM (`decode` already has a legacy plain-JSON
  fallback — extend the same pattern).
- Load: accept v1 (current) and v2; first save writes v2 (+ shards). Delete
  legacy per-ImageSet `<is>-images` CMs after the consolidation migration
  completes (the `//nolint:staticcheck // migration pending` sites in
  `cmd/main.go`, `cmd/worker/main.go`, `manager_resolve.go:1086`).
- Keep v1 decoding for one release; drop it together with the deprecated
  flat `Origin`/`EntrySig`/`OriginRef` fields on `ImageEntry`.
- Stage 2 ships behind a feature gate on the MirrorTarget
  (e.g. `spec.stateStore: ConfigMap | Manager`), defaulting to `ConfigMap`
  for one release.

## 7. Impact map (Stage 1)

| Area | Change |
|---|---|
| `pkg/mirror/imagestate/` | schema v2 encoder/decoder, shard-aware `Load/SaveForTarget`, dirty-shard tracking |
| `pkg/mirror/manager/manager.go` (Phase F) | pass per-entry dirty set instead of a single bool |
| `internal/controller/mirrortarget_controller.go` | cleanup snapshots via v2 `SaveRaw`; no logic change |
| `internal/controller/imageset_controller.go` | none (reads via package API) |
| `pkg/resourceapi/server.go`, `cmd/worker`, `cmd/main.go` | none (reads via package API) |
| RBAC | none (same ConfigMap verbs) |
