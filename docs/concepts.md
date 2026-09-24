# Concepts

This page is the mental model for everything else in the documentation: the three
custom resources, the pods that do the work, and what happens to a single image between
"declared in a spec" and "available in the target registry".

**Contents**

- [The resources](#the-resources)
- [The components](#the-components)
- [The life of an image](#the-life-of-an-image)
- [Where state lives](#where-state-lives)
- [Keeping the mirror current](#keeping-the-mirror-current)
- [Removing content](#removing-content)
- [Target registry layout](#target-registry-layout)
- [Glossary](#glossary)

---

## The resources

```
            ┌────────────────────┐   spec.imageSets   ┌──────────────────┐
            │   MirrorTarget     │ ─────────────────▶ │     ImageSet     │
            │ where + how        │        1 : n       │ what             │
            │ registry, creds,   │                    │ releases,        │
            │ concurrency, poll  │                    │ operators, helm, │
            │ intervals, proxy…  │                    │ additional imgs  │
            └────────────────────┘                    └──────────────────┘

            ┌────────────────────┐
            │   MirrorExport     │  what + destination registry, rendered once into
            │ artifacts only     │  downloadable artifacts; nothing is copied
            └────────────────────┘
```

| Resource | Answers | Notes |
|---|---|---|
| **`ImageSet`** | *What* should be mirrored | Release channels, operator catalogs and packages, Helm charts, additional images, blocked images. Reusable and inert on its own. |
| **`MirrorTarget`** | *Where* and *how* | Target registry and credentials, the list of `ImageSet`s to mirror there, worker concurrency, poll and drift-check intervals, proxy/CA, exposure of the Resource API. |
| **`MirrorExport`** | *Resolve, don't copy* | Same content model as an `ImageSet` plus a destination registry. Produces a manifest of source→destination pairs and the IDMS/ITMS/CatalogSource resources, for transfers by other tooling (air gaps). |

Three invariants:

- The **`MirrorTarget` owns the association**. An `ImageSet` never references a target.
- An `ImageSet` may be referenced by **exactly one** `MirrorTarget`. A second reference
  puts the `ImageSet` into `Ready=False/Unbound` with an explanatory message.
- All three resources and the secrets/ConfigMaps they use live in the **same namespace**
  — the operator's own namespace, since the operator is namespace-scoped.

## The components

The operator ships three container images that run as four kinds of workloads, plus two
optional ones on OpenShift.

```
 operator namespace
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  controller (1 Deployment)                                                │
 │   watches MirrorTarget / ImageSet / MirrorExport                          │
 │   ├─ per MirrorTarget: manager Deployment, RBAC, NetworkPolicies, Route   │
 │   ├─ per ImageSet with operators: catalog-build Job (once fully mirrored) │
 │   ├─ per removed ImageSet / orphaned images: cleanup Job                  │
 │   ├─ per MirrorExport: export-build Job                                   │
 │   └─ once: Resource API Deployment, console plugin, monitoring resources  │
 │                                                                           │
 │  manager (1 Deployment per MirrorTarget)                                  │
 │   resolves ImageSets → image list, owns the state ConfigMaps,             │
 │   dispatches worker pods, drift-checks the target registry,               │
 │   generates IDMS/ITMS/CatalogSource ConfigMaps, updates ImageSet.status   │
 │        │ creates                       ▲ POST /status, GET /should-mirror  │
 │        ▼                               │                                  │
 │  worker pods (ephemeral, ≤ concurrency at a time, batchSize images each)  │
 │   copy images source → target with regclient, report each result          │
 │                                                                           │
 │  jobs: catalog-build · cleanup · export-build                             │
 │  resource-api (1 Deployment per namespace, :8081) — serves generated YAML │
 │  console plugin (OpenShift only) — UI inside the OpenShift web console    │
 └───────────────────────────────────────────────────────────────────────────┘
```

| Workload | Image | Lifetime | Responsibility |
|---|---|---|---|
| **Controller** | `oc-mirror-operator-controller` | long-lived, 1 per install | Reconciles the CRs, creates everything below, aggregates status. |
| **Manager** | `oc-mirror-operator-manager` | long-lived, 1 per `MirrorTarget` | The brain of one mirror: resolve, dispatch, verify, publish. Reconciles every 30 s and immediately on worker callbacks. |
| **Worker** | `oc-mirror-operator-worker` | ephemeral, 1 per batch | Copies a batch of images. Asks the manager before each image whether it is still needed. Deleted after completion. |
| **Catalog-build Job** | `oc-mirror-operator-controller` (`catalog-builder` binary) | runs to completion | Filters the upstream FBC and pushes the filtered catalog image. |
| **Cleanup Job** | `oc-mirror-operator-worker` (`cleanup` subcommand) | runs to completion | Deletes images from the target registry. |
| **Export-build Job** | `oc-mirror-operator-controller` (`export-builder` binary) | runs to completion | Renders a `MirrorExport` into its artifacts ConfigMap. |
| **Resource API** | `oc-mirror-operator-manager` (`resource-api` subcommand) | long-lived, 1 per namespace | HTTP access to the generated resources and the edit API used by the console plugin. |
| **Console plugin** | `oc-mirror-operator-plugin` | long-lived, OpenShift only | Dashboard, catalog browser, editing of ImageSets/MirrorTargets. |

See [Architecture](architecture.md) for the internals of each.

## The life of an image

```
  ImageSet spec ──resolve──▶ Pending ──dispatch──▶ (worker copies) ──▶ Mirrored
                                ▲                        │                 │
                                │        failure         ▼                 │ drift check every
                                │◀──── retry (≤10×) ── Failed              │ checkExistInterval
                                │                        │ 10th failure    │
                                │                        ▼                 ▼
                                └──── registry check ── PermanentlyFailed  missing → Pending
```

1. **Resolve.** The manager turns the spec into a list of *(source, destination)* pairs:
   Cincinnati graph → release payloads → their component images; catalog image → FBC →
   filtered bundles → bundle and related images; Helm index → chart → rendered
   manifests → images; additional images as given. Every entry starts as `Pending`.
   Resolution is cached per spec entry (a signature over the entry plus the upstream
   digest), so an unchanged catalog is not re-parsed on every poll.
2. **Dispatch.** Pending images are grouped into batches of `batchSize` and handed to
   worker pods, at most `concurrency` pods at a time. Operator bundle images go first so
   OLM has something installable as early as possible.
3. **Copy.** A worker copies each image with regclient, including cosign `.sig` tags and
   OCI referrers, buffering layers larger than 100 MiB on local disk first. It verifies
   the pushed digest and reports `Mirrored` or `Failed` with the error to the manager.
4. **Retry.** A failed image is re-queued immediately; after 10 failures it becomes
   `PermanentlyFailed`, is listed in `status.failedImageDetails`, and is only retried by
   the drift check, a `recollect`, a `force-resync`, or a spec change.
5. **Verify.** Every `checkExistInterval` the manager checks each `Mirrored` image with a
   `HEAD` request against the target registry. Missing images go back to `Pending`.
   Tag-referenced additional images are additionally compared against their upstream
   digest, so a moved tag is re-mirrored. With `requireSignedImages`, a cosign signature
   is required at the destination as well.
6. **Build.** Once every image of an `ImageSet` is `Mirrored` or `PermanentlyFailed`, the
   controller starts a catalog-build Job per operator catalog, pinned to exactly the
   catalog digest the images were resolved from. A catalog never advertises a bundle
   that is not in the registry.
7. **Publish.** The manager writes IDMS, ITMS, `CatalogSource` and `ClusterCatalog`
   YAML into the ConfigMap `oc-mirror-<target>-resources`, from which the Resource API
   serves them.

## Where state lives

| Object | Owner | Content |
|---|---|---|
| `<imageset>-images` ConfigMap | manager | Gzip-compressed JSON: destination → `{source, state, retryCount, lastError, permanentlyFailed, origin, entrySig, originRef, …}`. ~30 bytes per image; 50 000 images fit comfortably under the 1 MiB ConfigMap limit. Also carries the resolved catalog digests as an annotation. |
| `<target>-images-index` ConfigMap | manager | Destinations referenced by more than one `ImageSet` (so cleanup never deletes a shared image). Only exists when something is shared. |
| `<target>-images-orphans` ConfigMap | manager → controller | Images that dropped out of every `ImageSet` (spec narrowing, blocked). Consumed by the cleanup Job when `cleanup-policy=Delete`. |
| `<target>-cleanup-<imageset>-<hash>` ConfigMap | controller | Snapshot handed to a cleanup Job. |
| `oc-mirror-<target>-resources` ConfigMap | manager | Generated IDMS/ITMS/CatalogSource/ClusterCatalog YAML plus `index.json`. |
| `oc-mirror-<target>-<slug>-packages` / `-upstream-packages` | manager | Package/channel/version listing of the filtered and the upstream catalog, for the console plugin. |
| `<target>-signatures` ConfigMap | manager | Verified GPG signatures of mirrored release payloads. |
| `<target>-worker-token` Secret | manager | Bearer token workers use for the status API. |
| `ImageSet.status` | manager (counts, `Ready`) and controller (`CatalogReady`) | Counters, conditions, `failedImageDetails`, `lastSuccessfulPollTime`, `observedGeneration`. |
| `MirrorTarget.status` | controller | Aggregated counters, per-ImageSet summary, `pendingCleanup`, conditions. |
| `ImageSet` annotations | manager | Resolution cache (`catalog-digest-<sig>`, `release-digest-<sig>`, `graph-image-built`); catalog build bookkeeping (`catalog-build-*`). Do not edit. |

Nothing needs a PersistentVolume; a manager restart reloads everything from the
ConfigMaps and re-adopts running worker pods.

## Keeping the mirror current

| Mechanism | Interval | What it does |
|---|---|---|
| **Reconcile tick** | 30 s (plus on every worker callback) | Dispatches pending images, flushes state, updates status. |
| **Upstream polling** | `pollInterval`, default 24 h, min 1 h, `0s` disables | Re-resolves every spec entry whose upstream changed: new releases in a channel, a republished catalog tag, a moved Helm chart version. New images become `Pending`, dropped images become orphans. Also rebuilds the OSUS graph image when enabled. |
| **Drift check** | `checkExistInterval`, default 6 h, min 1 h | Verifies every mirrored image still exists in the target (and, for tag-based additional images, still matches upstream). Runs in the background so dispatching continues. |
| **Spec change** | immediately | Any edit to an `ImageSet` or `MirrorTarget` triggers a resolve on the next tick. |
| **`recollect` annotation** | on demand | Forces a full re-resolution ignoring the cache, retries failed images, and requests a catalog rebuild. |
| **`force-resync` annotation** | on demand | Resets every image of an `ImageSet` to `Pending`, including mirrored ones. |

Details and commands are in [Operations](operations.md).

## Removing content

Removing an `ImageSet` from `spec.imageSets`, deleting a package, narrowing a version
range or blocking an image makes the affected images **orphans**. By default they stay
in the registry, untouched. If the `MirrorTarget` carries the annotation
`mirror.openshift.io/cleanup-policy: Delete`, the controller creates a cleanup Job that
deletes them — but never an image that another `ImageSet` of the same target still needs.

Deleting an `ImageSet` object directly (`kubectl delete imageset`) does **not** trigger
cleanup. Remove it from the `MirrorTarget` first, let the cleanup finish, then delete
the object. See [Operations → Cleanup](operations.md#cleanup).

## Target registry layout

Destinations follow oc-mirror v2 conventions so IDMS/ITMS rules stay simple:

| Content | Source | Destination under `spec.registry` |
|---|---|---|
| Release payload | `quay.io/openshift-release-dev/ocp-release@sha256:…` (4.16.20) | `openshift/release-images:4.16.20-x86_64` |
| Release component | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:…` (etcd) | `openshift/release:4.16.20-x86_64-etcd` |
| KubeVirt container disk | from the release payload | `openshift/release:4.16.20-x86_64-kube-virt-container` |
| Operator bundle / related image | `registry.redhat.io/ns/image@sha256:abc…` | `ns/image:sha256-abc…` (host stripped, digest becomes the tag) |
| Filtered catalog | `registry.redhat.io/redhat/redhat-operator-index:v4.16` | `registry.redhat.io/redhat/redhat-operator-index:v4.16` (full reference kept, overridable via `targetCatalog`/`targetTag`) |
| Additional image | `quay.io/org/app:1.2` | `quay.io/org/app:1.2` (full reference kept, overridable via `targetRepo`/`targetTag`) |
| Helm chart image | as rendered from the chart | full reference kept |
| OSUS graph data | built by the manager | `openshift/graph-image:latest` |

## Glossary

| Term | Meaning |
|---|---|
| **Resolve** | Turning a spec entry into concrete images (Cincinnati, FBC, Helm rendering). |
| **Entry signature** | Hash of one spec entry (channel or catalog entry) used as the cache key for its resolution. |
| **Heads-only** | Default operator filter: only the newest bundle of every channel of a package. |
| **Drift** | A mirrored image that is no longer in the target registry, or whose upstream tag moved. |
| **Orphan** | An image no `ImageSet` of the target references any more. |
| **Slug** | URL-safe catalog name, e.g. `redhat-operator-index-v4.16`, used in ConfigMap names and API paths. |
| **IDMS / ITMS** | `ImageDigestMirrorSet` / `ImageTagMirrorSet`: OpenShift resources redirecting pulls to the mirror. |
| **FBC** | File-based catalog, the JSON/YAML format of OLM operator catalogs. |
