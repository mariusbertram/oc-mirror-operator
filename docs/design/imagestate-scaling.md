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

#### B′. Fully binary interned encoding — and the digest floor

Schema v2 can go further than compact JSON: a binary encoding (CBOR or a
hand-rolled layout) with interned tables — repo paths and ref tuples stored
once, entries referencing them by varint index, digests stored as 32 raw bytes
instead of 64 hex characters. Per entry that leaves roughly:

| Field | Bytes |
|---|---|
| digest (raw) | 32 |
| repo-table index + tag/exception flag | 2–4 |
| state + retryCount + permanentlyFailed (packed) | 1 |
| group indices (`refs`) | 1–3 |

≈ 36–45 B/image plus amortized tables — about **2–2.5×** better than the
current gzip-JSON (~60–80 B/image observed).

Losing human readability costs little: the current format is *already* opaque
(`images.json.gz` in `BinaryData` needs base64 + gunzip to inspect), and the
practical debug views are the dashboard and resource API. A
`state dump` debugging subcommand on the main binary (render any schema
version as JSON) preserves operability regardless of encoding.

The hard limit, however, is information-theoretic: a sha256 digest is 32 bytes
of pure entropy per image. **No encoding gets below O(images × 32 B)** while
per-image digests are stored, so 1 MiB caps out around ~25–30 k images. A
binary format raises the ceiling; it does not remove it.

Getting under that floor means the store stops carrying full image identities
and becomes a state annex to the (deterministic) resolution. Two variants:

**B″ — truncated digest keys.** Hashing a sha256 digest is mathematically
just truncation (the digest is already uniform), but truncation is enough for
a *state key*: at 8 bytes (64 bits) the birthday-bound collision probability
across 100 k images is ~10⁻¹⁰ — negligible, and including the repo index in
the key confines any collision to a single repo. Entries shrink to
~12–15 B/image (8 B key + repo index + packed flags + group index):
**~70–80 k images per MiB**. Crucially this only needs deterministic
*membership*, not ordering — resolution output is matched to prior state by
key lookup, which the manager-restart carry-over
(`manager_resolve.go`) effectively already does with full-ref keys.

**B‴ — per-group state bitmap.** Store per `(entrySig, catalog digest)` group
only a 2-bit state bitmap over the deterministically-*ordered* resolved list,
plus full exception records for Pending/Failed. O(spec) + O(deviations) — a
few KiB even at 100 k images — but stable resolution ordering becomes a hard
invariant, making it the more fragile of the two.

Both variants share the same structural consequence: without full refs in the
ConfigMap, every consumer that needs actual image references must get them
from a component that has run resolution — cleanup worklists must be produced
by the manager (or regenerated via registry listing) instead of partitioned
from the CM by the controller, and the dashboard must query the manager
instead of decoding the CM. The gate check and counts keep working (they only
need sigs and states). In other words, B″/B‴ quietly pull in the
consumer-topology half of option C while keeping etcd in the write path —
which is why C remains the preferred end-state, with B′ (and optionally B″)
scoped as encoding refinements of Stage 1.

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

## 5. Recommendation — tiered store with threshold escalation

The end state combines B and C as a **tiered backend behind one interface**,
so small mirrors run with zero storage dependencies and very large ones
migrate to a PVC when they outgrow the ConfigMap:

```yaml
spec:
  stateStore: Auto        # Auto (default) | ConfigMap | PVC
```

- **ConfigMap tier** (small/medium): full state persisted as the schema-v2
  compact blob (optionally sharded). With v2 encoding, ~700 KiB covers
  ≳15–20 k images — most installations never leave this tier.
- **PVC tier** (large): bbolt store on a small RWO PVC mounted into the
  manager Deployment (strategy `Recreate` to avoid multi-attach on rollout);
  per-entry writes, no size ceiling. Because the state is reconstructible
  (§3), PVC loss is a cold start, not data loss.

The precondition that keeps this from becoming two parallel code paths:
**consumers never read the full blob in either tier.** Stage 2's consumer
topology ships first and applies to both tiers — the manager owns the full
state in memory, controllers and dashboard read the small summary-checkpoint
CM (`<mt>-state-summary`: counts + gate sigs) plus the manager's HTTP
endpoints, and cleanup worklists are manager-produced. The tier then only
changes how the *manager* persists its own state, hidden entirely behind a
store interface in `pkg/mirror/imagestate/`.

### 5.1 Auto-escalation

The manager knows the encoded size at every flush:

1. Below the threshold (default ~60 % of 1 MiB, configurable): ConfigMap tier.
2. Crossing it: emit a metric + Event and set a MirrorTarget condition
   (`StateStoreScale=True/reason=ApproachingConfigMapLimit`).
   - If `stateStore: Auto` **and** a usable StorageClass exists: the operator
     provisions the PVC (owner-ref'd to the MirrorTarget), patches the manager
     Deployment, and the manager imports the CM blob into bbolt on startup.
     Only after a verified import is the state CM truncated to the summary
     checkpoint. One-way with hysteresis — no automatic downgrade.
   - If no StorageClass is available (or `stateStore: ConfigMap` is pinned):
     the condition remains as an actionable warning; sharding keeps the
     installation running in the meantime.
3. `stateStore: PVC` pins the large tier from the start for deployments that
   know they are big (e.g. 15 ImageSets × 3 catalogs).

Failure handling: the CM blob is kept intact until the bbolt import is
verified, so a failed migration falls back to the ConfigMap tier; threshold
flapping is prevented by escalating on a sustained crossing (N consecutive
flushes) and never de-escalating automatically.

### 5.2 Staging

**Stage 1 (next release, low risk):** schema v2 interned encoding + sharding
behind the existing package API (options B/A); no consumer outside
`pkg/mirror/imagestate/` changes. Buys ~5–10× headroom and cuts etcd churn
immediately — and the encoder is reused later for the summary checkpoint and
cleanup snapshots.

**Stage 2 (architectural):** consumer topology of option C — summary
checkpoint CM, manager HTTP read endpoints, manager-produced cleanup
worklists — while the full state still persists in the (v2) ConfigMap.

**Stage 3:** the PVC tier + `spec.stateStore` + auto-escalation per §5.1,
which at that point is only a new persistence backend inside the manager.
D remains an optional export format later.

## 6. Migration

- Add `schemaVersion` to the CM (`decode` already has a legacy plain-JSON
  fallback — extend the same pattern).
- Load: accept v1 (current) and v2; first save writes v2 (+ shards). Delete
  legacy per-ImageSet `<is>-images` CMs after the consolidation migration
  completes (the `//nolint:staticcheck // migration pending` sites in
  `cmd/main.go`, `cmd/worker/main.go`, `manager_resolve.go:1086`).
- Keep v1 decoding for one release; drop it together with the deprecated
  flat `Origin`/`EntrySig`/`OriginRef` fields on `ImageEntry`.
- `spec.stateStore` (`Auto | ConfigMap | PVC`) is introduced in Stage 3 and
  defaults to `Auto`; the Auto-escalation path (§5.1) doubles as the
  CM→PVC migration mechanism, so no separate migration tooling is needed.
  Pinning `ConfigMap` opts out for clusters without a StorageClass.

## 7. Impact map (Stage 1)

| Area | Change |
|---|---|
| `pkg/mirror/imagestate/` | schema v2 encoder/decoder, shard-aware `Load/SaveForTarget`, dirty-shard tracking |
| `pkg/mirror/manager/manager.go` (Phase F) | pass per-entry dirty set instead of a single bool |
| `internal/controller/mirrortarget_controller.go` | cleanup snapshots via v2 `SaveRaw`; no logic change |
| `internal/controller/imageset_controller.go` | none (reads via package API) |
| `pkg/resourceapi/server.go`, `cmd/worker`, `cmd/main.go` | none (reads via package API) |
| RBAC | none (same ConfigMap verbs) |
