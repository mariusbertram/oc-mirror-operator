# Architecture

How the pieces fit together, for operators who want to understand what runs in their
cluster and for contributors. [Concepts](concepts.md) covers the user-facing model;
this page goes one level deeper.

**Contents**

- [Process model](#process-model)
- [Controller](#controller)
- [Manager reconcile loop](#manager-reconcile-loop)
- [Workers](#workers)
- [Resolution and caching](#resolution-and-caching)
- [Operator catalog pipeline](#operator-catalog-pipeline)
- [Blob replication planning](#blob-replication-planning)
- [State model](#state-model)
- [Security model](#security-model)
- [Repository layout](#repository-layout)

---

## Process model

One code base, three images, several roles:

| Image | Binary / subcommand | Runs as |
|---|---|---|
| `oc-mirror-operator-controller` | `controller` | The operator Deployment (`oc-mirror-operator-controller-manager`) |
| | `catalog-builder` | Catalog-build Jobs |
| | `export-builder` | MirrorExport Jobs |
| `oc-mirror-operator-manager` | `manager --mirrortarget <name>` | One Deployment per MirrorTarget |
| | `resource-api --namespace <ns>` | The Resource API Deployment (one per namespace) |
| `oc-mirror-operator-worker` | `worker` (batch from `MIRROR_BATCH`) | Ephemeral worker pods |
| | `cleanup --configmap <snapshot>` | Cleanup Jobs |
| `oc-mirror-operator-plugin` | console plugin backend + static UI | `oc-mirror-plugin` Deployment (OpenShift) |

```
                         ┌──────────────────────────┐
   MirrorTarget ───────▶ │ controller               │ ──▶ manager Deployment, Services,
   ImageSet     ───────▶ │  MirrorTargetReconciler  │     RBAC, NetworkPolicies, Route/Ingress
   MirrorExport ───────▶ │  ImageSetReconciler      │ ──▶ catalog-build Jobs
   ConfigMaps  (watch)   │  MirrorExportReconciler  │ ──▶ cleanup Jobs, export Jobs
                         │  ConsolePluginReconciler │ ──▶ plugin Deployment + ConsolePlugin CR
                         │  MonitoringReconciler    │ ──▶ ServiceMonitors, PrometheusRule, dashboard
                         └──────────────────────────┘

   ┌───────────────────── manager (per MirrorTarget) ─────────────────────┐
   │ 30 s tick / urgent flush                                             │
   │  A load state ─ B resolve ImageSets ─ C drift sweep (bg) ─ D classify│
   │  E dispatch batches ─ F flush ConfigMaps ─ G ImageSet.status ─ H IDMS│
   │  :8080 /status /should-mirror (worker token)   :9090 /metrics /healthz│
   └──────────────────────────────────────────────────────────────────────┘
                 │ Pod create                      ▲ HTTP
                 ▼                                 │
          worker pod: PlanMirrorOrder → for each image: should-mirror? → copy → verify → POST /status
```

## Controller

`internal/controller/` holds five reconcilers in one controller-runtime manager. The
operator is namespace-scoped: informers are restricted to the operator namespace (plus
`openshift-config-managed` for the dashboard ConfigMap).

| Reconciler | Watches | Creates / maintains |
|---|---|---|
| `MirrorTargetReconciler` | MirrorTarget, ImageSet status (for aggregation), owned objects | Coordinator/worker ServiceAccounts, Roles, RoleBindings; NetworkPolicies; the manager Deployment and Service; the Resource API Deployment/Service/RBAC (shared per namespace); Route/Ingress/HTTPRoute; cleanup Jobs; `status` aggregation; finalizer that deletes the manager and waits for its pods on deletion. |
| `ImageSetReconciler` | ImageSet, MirrorTarget (to requeue its ImageSets), `*-images` ConfigMaps | Catalog-build Jobs, gated on the ImageSet being fully mirrored and pinned to the catalog digest stored with the state; the `CatalogReady` condition; `catalog-build-*` bookkeeping annotations. |
| `MirrorExportReconciler` | MirrorExport, owned Job/ConfigMap/RBAC | Export ServiceAccount/Role scoped to one ConfigMap, the artifacts ConfigMap, the export-build Job; `Ready` condition and `status.totalImages`. |
| `ConsolePluginReconciler` | The `ConsolePlugin` CR (cluster-scoped, OpenShift only) | Plugin Deployment, Service, RBAC, the `ConsolePlugin` CR with a cleanup finalizer. Skipped when the CRD or `PLUGIN_IMAGE` is absent. |
| `MonitoringReconciler` | timer (10 min) | `oc-mirror-controller` and `oc-mirror-manager` ServiceMonitors, the `PrometheusRule`, the Grafana/console dashboard ConfigMap. Skipped without prometheus-operator CRDs. |

Conditions are written through one helper (`setCondition`) that only bumps
`lastTransitionTime` when the status flips and always records `observedGeneration`.

## Manager reconcile loop

`pkg/mirror/manager`. One process per MirrorTarget, reconciling every 30 s and
immediately after a worker callback (`urgentFlush`). Each tick (`reconcile()`):

| Phase | Work |
|---|---|
| **cleanup** | List worker pods; drop finished ones from `inProgress`; reset images of failed/stuck (Pending > 15 min) pods to `Pending`; delete finished pods. |
| **A load** | On first tick, load every referenced ImageSet's `*-images` ConfigMap (and migrate a legacy consolidated map) into the in-memory `imageState`/`owners`. |
| **B resolve** | For each ImageSet in `spec.imageSets`: honour `force-resync`; if `shouldResolve()` (empty state, `recollect`, generation change, stale cache version, poll interval elapsed), run `resolveImageSet` outside the mutex with a 1 h timeout and merge the result. |
| **C drift** | Every `checkExistInterval`, start a background sweep (20 parallel `HEAD`s, 2 min timeout each) over mirrored and permanently failed images. |
| **D classify** | Orphan entries → `<target>-images-orphans`; `Failed` with < 10 retries → `Pending`; collect pending images, bundle images first. |
| **E dispatch** | Create worker pods in batches of `batchSize` up to `concurrency`. |
| **F flush** | Write each ImageSet's ConfigMap (entries + resolved catalog digests in one update) and the shared index. |
| **G status** | Update `ImageSet.status` counts, `Ready`, `failedImageDetails` (max 20), `observedGeneration`/`lastSuccessfulPollTime` on a clean resolve. |
| **H publish** | Regenerate IDMS/ITMS/CatalogSource/ClusterCatalog into `oc-mirror-<target>-resources`. |

A heartbeat is written around each tick and exposed on `/healthz` (port 9090); the
Deployment's liveness probe restarts a manager whose loop has been silent for 1.5 h.

The status API on port 8080 (`/status`, `/should-mirror`) is protected by a bearer token
generated once and stored in `<target>-worker-token`; comparison is constant-time.

## Workers

`cmd/worker`. A worker receives its batch as JSON in `MIRROR_BATCH`, computes the copy
order (see [Blob replication planning](#blob-replication-planning)), and for each image:

1. `GET /should-mirror?dest=…` — skip if the manager answers `410 Gone` (image removed
   from the spec or already mirrored).
2. Copy with regclient (`ImageCopy` with referrers), buffering layers > 100 MiB on
   `/tmp/blob-buffer` and using monolithic `PUT`s against the target. Two attempts, 20 min
   each. Cosign `sha256-<digest>.sig` tags are copied best-effort.
3. `HEAD` the pushed manifest to verify the digest.
4. `POST /status` with the result (three attempts).

The registry client is recreated every 20 images to keep the accumulated bearer-token
scope below Quay's 8 KB header limit. A pod exits 0 even if images failed; failure is
reported per image.

## Resolution and caching

`pkg/mirror/collector.go` and `pkg/mirror/{release,catalog,helm}`.

| Source | Resolver | Cache key | Cached value |
|---|---|---|---|
| Release channel | Cincinnati graph → `ResolveReleaseNodes` (full / min / max / shortest path) → GPG verification → payload `image-references` extraction | `ReleaseChannelSignature` (name, type, range, flags, arches, kubevirt) | Hash of the resolved payload list, in annotation `release-digest-<sig>` |
| Operator entry | `GetCatalogDigest` → pinned pull → FBC parse → `FilterFBC` (heads-only / channels / ranges, BFS dependencies) → bundle + related images | `OperatorEntrySignature` (catalog, target overrides, flags, packages with channels/ranges) | `v5:<catalog digest>` in annotation `catalog-digest-<sig>` |
| Helm chart | index → download → `helm template` → JSONPath scan | none (re-rendered every resolve) | — |
| Additional images | pass-through | none | — |
| Graph image | download graph-data, build layer, push | `graph-image-built` timestamp | rebuilt per `pollInterval` |

On a cache hit the previous entries carrying the same `(origin, entrySig)` are carried
over unchanged, so a 24 h poll against an unchanged catalog costs one `HEAD` request.
Every `ImageEntry` records its origin, signature and a human-readable `originRef`, which
is what `failedImageDetails.origin` shows.

## Operator catalog pipeline

1. **Filter** (`pkg/mirror/catalog/resolver.go`): companion packages are added
   (`<pkg>-dependencies`), heads are computed per channel (`channelHeadPlusN`), version
   filters applied, and `olm.package.required`/`olm.gvk.required` dependencies followed
   transitively. Channel graphs are repaired so each channel keeps exactly one head, and
   a package's default channel is repointed if the original was filtered out — both
   avoid `opm` validation failures.
2. **Gate** (`ImageSetReconciler`): a build Job is created only when every image of
   the ImageSet is `Mirrored` or permanently failed and the state reflects the current
   spec. The Job pulls the catalog **by the digest recorded with the state**, never by
   tag, so it cannot see content newer than what was mirrored.
3. **Build** (`catalog-builder`): the source catalog is copied to a local OCI layout,
   its layers are classified, the filtered FBC is written as a new layer with an opaque
   whiteout over `/configs` and `/tmp/cache`, and the result is pushed to
   `resources.CatalogTargetImage` (source reference under the target, or
   `targetCatalog`/`targetTag`). The image keeps `opm`, its entrypoint and the OLM label,
   so it serves as a `CatalogSource` (gRPC) and a `ClusterCatalog`.
4. **Rebuild** happens on a changed build signature (packages/channels/ranges), a
   changed resolved catalog digest, a poll expiry, or a recollect — always through the
   same gate.

## Blob replication planning

`pkg/mirror/planner.go`. Before copying, a worker fetches the manifests of its batch,
counts how many images reference each blob, and orders the batch greedily: the image
sharing the most blobs first, then whichever image has the most already-uploaded blobs.
Blobs already in the target are found by regclient's cross-repository mount, so shared
base layers are transferred once per batch instead of once per image.

## State model

See [Concepts → Where state lives](concepts.md#where-state-lives) for the list of
objects. Design notes:

- State is **partitioned per ImageSet** (`<imageset>-images`) with a small
  **shared-image index** (`<target>-images-index`) listing destinations referenced by
  more than one ImageSet. Cleanup consults the index (and the live state of remaining
  ImageSets) so a shared image is never deleted. Rationale and consistency rules are in
  [`design/imagestate-per-imageset-partitioning.md`](design/imagestate-per-imageset-partitioning.md).
- Writes across several ConfigMaps are not atomic; a crash between them is repaired on
  the next flush. The ImageSet's resolved catalog digests are stored **in the same
  ConfigMap update** as its entries so the catalog gate never pairs a new digest with
  old entries.
- `permanentlyFailed` is a sticky marker: it survives a reset to `Pending` so the catalog
  gate stays open and the image keeps appearing in `failedImageDetails` until it is
  mirrored.

## Security model

| Aspect | Implementation |
|---|---|
| Scope | Namespace-scoped operator; `Role`s, no `ClusterRole` (except what the console plugin needs). |
| Per-target RBAC | `<target>-coordinator` (manager: ImageSets status, pods, ConfigMaps, worker-token secret, PVCs) and `<target>-worker` (no API access) ServiceAccounts, owned by the MirrorTarget. |
| Resource API | Runs as `oc-mirror-resource-api` with read-only access. Write endpoints act with the **caller's** bearer token (from the console), so cluster RBAC decides. |
| Pod security | All pods: `runAsNonRoot`, `allowPrivilegeEscalation: false`, drop `ALL`, seccomp `RuntimeDefault`. |
| Worker → manager | Bearer token in a Secret, injected via `secretKeyRef`, constant-time comparison. |
| NetworkPolicies | `<target>-manager-ingress`: 8080 only from that target's workers, 8081/9090 open in-cluster. `<target>-worker-ingress-deny`: no ingress to workers. Egress is not restricted (registry and DNS topologies vary too much); add your own policy on the worker selector if needed. |
| Credentials | `authSecret` mounted as a volume (`config.json` key only), never in env or logs. |
| Content trust | Release payloads GPG-verified against embedded Red Hat keys; optional cosign key pinning per catalog; optional signature presence check per ImageSet; cosign signatures and referrers copied along. |
| Supply chain | Pinned base images, VEX statements under `vex/`, `grype` and CodeQL in CI. |

## Repository layout

```
api/v1alpha1/            CRD types (MirrorTarget, ImageSet, MirrorExport), signatures, deepcopy
cmd/
  controller/            operator entrypoint
  manager/               manager + resource-api subcommand
  worker/                worker + cleanup subcommand
  catalog-builder/       catalog-build Job binary
  export-builder/        MirrorExport Job binary
  dashboard/             console plugin backend (serves the UI + API with the caller's token)
  main.go                legacy all-in-one binary (deprecated)
internal/controller/     the five reconcilers
pkg/mirror/
  manager/               reconcile loop, resolve, drift sweep, status API
  collector.go           spec → (source, destination) pairs
  planner.go             blob-aware mirror ordering
  client/                regclient wrapper: copy, buffering, TLS fallback, delete
  release/               Cincinnati, payload extraction, KubeVirt disks
  catalog/               FBC loading, filtering, catalog image build; builder/ = Job management
  helm/                  chart download and rendering
  imagestate/            ConfigMap-backed state, shared index, migration
  resources/             IDMS/ITMS/CatalogSource/ClusterCatalog/package listings
  export/                MirrorExport manifest and artifacts; builder/ = Job management
  cosign/, graph/        cosign verification, OSUS graph image
pkg/release/             GPG signature download and verification
pkg/resourceapi/         REST API server (+ embedded plugin assets)
pkg/metrics/             Prometheus metrics
ui/                      console plugin (React, PatternFly)
config/                  kustomize: CRDs, RBAC, manager, samples, OLM CSV base
bundle/, catalog/        generated OLM bundle and catalog (do not edit)
test/e2e/                Ginkgo end-to-end suites
docs/                    this documentation
```
