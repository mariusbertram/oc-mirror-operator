# API reference

API group `mirror.openshift.io/v1alpha1`. Three kinds: [`MirrorTarget`](#mirrortarget),
[`ImageSet`](#imageset), [`MirrorExport`](#mirrorexport). All are namespaced and must
live in the operator's namespace.

Annotated field tables are generated from `api/v1alpha1/*_types.go`; the CRD YAML in
`config/crd/bases/` is authoritative for validation rules.

---

## MirrorTarget

Target registry plus the list of ImageSets mirrored into it, and everything about how
the mirroring runs. See [Configuring MirrorTargets](../configuration/mirrortarget.md)
for guidance.

### Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `registry` | string | **required** | Target registry with optional path: `host[:port]/path`. No scheme. Prefix of every destination reference. |
| `imageSets` | []string | `[]` | Names of ImageSets (same namespace) to mirror. Each ImageSet may be listed by one MirrorTarget only. |
| `authSecret` | string | — | Secret with registry credentials (`kubernetes.io/dockerconfigjson`, or Opaque with `username`/`password`). Mounted into manager, worker, catalog-build and cleanup pods. |
| `insecure` | bool | `false` | Target registry: try plain HTTP first, then HTTPS without certificate verification. |
| `concurrency` | int | `1` | Worker pods running concurrently. 1–100. |
| `batchSize` | int | `50` | Images per worker pod. 1–100. |
| `pollInterval` | duration | `24h` | Upstream re-resolution interval. `0s` disables polling; otherwise ≥ `1h` (CEL-validated). |
| `checkExistInterval` | duration | `6h` | Drift-check interval against the target registry. ≥ `1h`. |
| `expose` | [ExposeConfig](#exposeconfig) | auto | How the Resource API is exposed for this target. |
| `manager` | [PodConfig](#podconfig) | — | Resources, node selector, tolerations for the manager pod. |
| `worker` | [PodConfig](#podconfig) | — | Same for worker pods and cleanup Jobs. |
| `proxy` | [ProxyConfig](#proxyconfig) | — | HTTP(S) proxy for all pods created for this target. |
| `caBundle` | [CABundleRef](#cabundleref) | — | Additional CA certificates for all pods created for this target. |
| `workerStorage` | [WorkerStorageConfig](#workerstorageconfig) | emptyDir 10Gi | Replace the worker blob buffer with an ephemeral PVC. |

#### ExposeConfig

| Field | Type | Description |
|---|---|---|
| `type` | `Route` \| `Ingress` \| `GatewayAPI` \| `Service` | Default: `Route` when the Route API exists (OpenShift), else `Service`. |
| `host` | string | Hostname for Route/Ingress/HTTPRoute. Optional for Route. |
| `ingressClassName` | string | `Ingress` only. |
| `gatewayRef.name` | string | `GatewayAPI` only, required: the Gateway to attach to. |
| `gatewayRef.namespace` | string | Gateway namespace; defaults to the MirrorTarget's. |

#### PodConfig

| Field | Type | Description |
|---|---|---|
| `resources` | `corev1.ResourceRequirements` | Requests and limits. |
| `nodeSelector` | map[string]string | |
| `tolerations` | []`corev1.Toleration` | |

#### ProxyConfig

| Field | Type | Description |
|---|---|---|
| `httpProxy` | string | `HTTP_PROXY`/`http_proxy`. |
| `httpsProxy` | string | `HTTPS_PROXY`/`https_proxy`. |
| `noProxy` | string | Extra comma-separated `NO_PROXY` entries. `localhost,127.0.0.1,.svc,.svc.cluster.local` are always prepended when a proxy is set, and `KUBERNETES_SERVICE_HOST` is rewritten to `kubernetes.default.svc.cluster.local`. |

#### CABundleRef

| Field | Type | Description |
|---|---|---|
| `configMapName` | string | **Required.** ConfigMap in the same namespace holding a PEM bundle. |
| `key` | string | Key in the ConfigMap. Default `ca-bundle.crt`. Mounted at `/run/secrets/ca/<key>`; `SSL_CERT_FILE` points at it. |

#### WorkerStorageConfig

| Field | Type | Description |
|---|---|---|
| `size` | quantity | Requested capacity (default `10Gi`). Must hold the largest single layer. |
| `storageClassName` | string | StorageClass; cluster default when omitted. Setting any `storageClassName` switches from emptyDir to a generic ephemeral PVC. |

### Annotations

| Annotation | Value | Effect |
|---|---|---|
| `mirror.openshift.io/cleanup-policy` | `Delete` | Delete images from the target registry when they are no longer referenced by any ImageSet of this target (ImageSet removed from `spec.imageSets`, spec narrowed, image blocked). Without it nothing is deleted. |

### Status

| Field | Type | Description |
|---|---|---|
| `conditions` | []Condition | `Ready`, `Cleanup` — see below. |
| `totalImages`, `mirroredImages`, `pendingImages`, `failedImages` | int | Counts deduplicated across all ImageSets (`failedImages` = permanently failed). |
| `imageSetStatuses[]` | list | Per ImageSet: `name`, `found`, `total`, `mirrored`, `pending`, `failed`. Sorted by name. |
| `knownImageSets` | []string | Last observed `spec.imageSets`, used to detect removals. Internal. |
| `pendingCleanup` | []string | ImageSets (or `orphans`) with a cleanup Job in progress. |

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| `Ready` | True | `DeploymentReady` | Manager Deployment and exposure reconciled. |
| `Ready` | False | `ReconcileError` | RBAC, NetworkPolicy, Resource API or Deployment reconcile failed (message). |
| `Ready` | False | `ExposureError` | Route/Ingress/HTTPRoute could not be created; status is still aggregated. |
| `Cleanup` | False | `CleanupInProgress` | Cleanup Jobs running. |
| `Cleanup` | True | `CleanupComplete` | Cleanup finished. |
| `Cleanup` | False | `CleanupError` | Cleanup Job creation failed. |

### Generated objects

Per MirrorTarget `<name>` (all owned by it and garbage-collected with it):

| Kind | Name | Purpose |
|---|---|---|
| Deployment, Service | `<name>-manager` | Manager pod; Service ports 8080 (status API) and 9090 (metrics). |
| Service | `<name>-resources` | Points at the shared Resource API Deployment (port 8081). |
| Route / Ingress / HTTPRoute | `<name>-resources` | Per `spec.expose`. |
| ServiceAccount, Role, RoleBinding | `<name>-coordinator` | Manager identity: ImageSets + status, MirrorTargets (read), Pods, ConfigMaps, worker-token Secret, PVCs. |
| ServiceAccount, Role, RoleBinding | `<name>-worker` | Worker identity, no API permissions. |
| NetworkPolicy | `<name>-manager-ingress`, `<name>-worker-ingress-deny` | See [Network requirements](network.md). |
| Secret | `<name>-worker-token` | Bearer token for the status API (created by the manager). |
| ConfigMap | `oc-mirror-<name>-resources`, `<name>-signatures`, `<name>-images-index`, `<name>-images-orphans`, `oc-mirror-<name>-<slug>-packages`, `…-upstream-packages` | See [Concepts → Where state lives](../concepts.md#where-state-lives). |
| Job | `cleanup-<name>-<imageset>-<hash>` | Cleanup Jobs. |

Per namespace (owned by every MirrorTarget in it, so they outlive any single one):
Deployment/Service `oc-mirror-resource-api` and ServiceAccount/Role/RoleBinding
`oc-mirror-resource-api` (read-only).

---

## ImageSet

What to mirror. Bound to a target only through `MirrorTarget.spec.imageSets`. See
[Configuring ImageSets](../configuration/imagesets.md) for guidance.

### Spec

`spec.mirror` is a [Mirror](#mirror) block.

#### Mirror

| Field | Type | Description |
|---|---|---|
| `platform` | [Platform](#platform) | OpenShift/OKD releases. |
| `operators` | [][Operator](#operator) | OLM catalogs. |
| `additionalImages` | [][AdditionalImage](#additionalimage) | Individual images. |
| `helm` | [Helm](#helm) | Helm chart images. |
| `blockedImages` | []`{name}` | Images to exclude from every other section. `name` is a repository path without registry, optionally with `:tag` or `@digest`. |
| `requireSignedImages` | bool | Require a cosign signature at every mirrored destination (presence check during the drift sweep; failing images become `Failed`). |
| `samples` | list | **Not implemented.** |

#### Platform

| Field | Type | Description |
|---|---|---|
| `channels` | [][ReleaseChannel](#releasechannel) | |
| `architectures` | []string | `amd64` (default), `arm64`, `s390x`, `ppc64le`. Each listed architecture is resolved and mirrored separately. |
| `graph` | bool | Build and push the OSUS graph-data image `openshift/graph-image:latest`. |
| `kubeVirtContainer` | bool | Also mirror the KubeVirt container-disk image of each release. |
| `release` | string | **Not implemented** (disk-to-mirror). |

#### ReleaseChannel

| Field | Type | Description |
|---|---|---|
| `name` | string | **Required.** e.g. `stable-4.16`, `fast-4.16`, `eus-4.16`, `candidate-4.16`. |
| `type` | `ocp` \| `okd` | Default `ocp`. |
| `minVersion` / `maxVersion` | string | Inclusive bounds; see the selection table in [Configuring ImageSets](../configuration/imagesets.md#selecting-versions). |
| `shortestPath` | bool | With both bounds set: only the releases on the shortest upgrade path. |
| `full` | bool | Every release in the channel. |
| `skipSignatureVerification` | bool | Skip GPG verification against the embedded Red Hat keys. Needed for OKD, CI and nightly payloads. |

#### Operator

| Field | Type | Description |
|---|---|---|
| `catalog` | string | **Required.** Catalog image: `repo:tag`, `repo@sha256:…` or `repo:tag@sha256:…`. |
| `packages` | [][IncludePackage](#includepackage) | Packages to include. Omit with `full: true` for the whole catalog. |
| `full` | bool | Mirror every package. |
| `skipDependencies` | bool | Do not follow `olm.package.required` / `olm.gvk.required` / companion packages. |
| `targetCatalog` | string | Path under the target registry for the filtered catalog image (default: the full source reference). |
| `targetTag` | string | Tag of the filtered catalog image (default: the source tag). |
| `signatureVerification.publicKeySecretRef` | `{name, key}` | Secret key holding a PEM cosign public key; the catalog must verify against it before every resolution. |

#### IncludePackage

| Field | Type | Description |
|---|---|---|
| `name` | string | **Required.** |
| `channels` | [][IncludeChannel](#includechannel) | Restrict to these channels (all their versions unless bounded). |
| `defaultChannel` | string | Default channel to advertise in the filtered catalog. |
| `previousVersions` | int | Heads-only mode only: older bundles to keep behind each channel head. Default 0. |
| `minVersion` / `maxVersion` | string | Bundle version bounds across all channels. |

Heads-only mode applies when `channels`, `minVersion` and `maxVersion` are all unset.

#### IncludeChannel

| Field | Type | Description |
|---|---|---|
| `name` | string | **Required.** |
| `minVersion` / `maxVersion` | string | Bounds within this channel (override the package-level bounds). |

#### AdditionalImage

| Field | Type | Description |
|---|---|---|
| `name` | string | **Required.** Full reference, tag or digest. |
| `targetRepo` | string | Repository path under the target registry (default: full source reference). |
| `targetTag` | string | Tag at the destination. |

#### Helm

| Field | Type | Description |
|---|---|---|
| `repositories[].name` | string | Display name. |
| `repositories[].url` | string | Repository base URL (`<url>/index.yaml`). |
| `repositories[].charts[].name` | string | Chart name. |
| `repositories[].charts[].version` | string | Chart version; empty = latest non-prerelease. |
| `repositories[].charts[].imagePaths` | []string | Extra JSONPath expressions evaluated on every rendered manifest. |
| `local` | list | **Not implemented.** |

### Annotations

Set by users:

| Annotation | Effect |
|---|---|
| `mirror.openshift.io/recollect` | One-shot. Re-resolve everything ignoring the cache, retry failed images, rebuild catalogs once mirrored. Removed by the manager when honoured. Any value; change it to trigger again. |
| `mirror.openshift.io/force-resync` | One-shot. Reset every image of the ImageSet to `Pending`, including mirrored ones. Removed by the manager when applied. |

Set by the operator (do not edit):

| Annotation | Owner | Purpose |
|---|---|---|
| `mirror.openshift.io/catalog-digest-<sig>` | manager | Resolution cache per operator entry (`v5:<digest>`). |
| `mirror.openshift.io/release-digest-<sig>` | manager | Resolution cache per release channel. |
| `mirror.openshift.io/graph-image-built` | manager | Timestamp of the last graph-image build. |
| `mirror.openshift.io/recollect-honored` | manager | Marker of the last honoured recollect, consumed by the catalog rebuild logic. |
| `mirror.openshift.io/catalog-build-sig`, `…-digests`, `…-recollect-sig`, `…-poll-sig` | controller | Inputs of the last catalog build, to decide when to rebuild. |

### Status

| Field | Type | Description |
|---|---|---|
| `conditions` | []Condition | `Ready`, `CatalogReady` — see below. |
| `totalImages`, `mirroredImages`, `pendingImages`, `failedImages` | int | Counts for this ImageSet (`failedImages` = permanently failed). |
| `failedImageDetails[]` | list | Up to 20 permanently failed images: `source`, `destination`, `error`, `origin`. |
| `observedGeneration` | int64 | Spec generation last resolved without upstream errors. |
| `lastSuccessfulPollTime` | time | When that resolve happened; the poll clock. |

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| `Ready` | True | `Collected` | Resolved (counts in the message). |
| `Ready` | False | `Empty` | Nothing resolved yet. |
| `Ready` | False | `Unbound` | Zero or several MirrorTargets reference this ImageSet. |
| `CatalogReady` | True | `CatalogBuildSucceeded` | All catalog-build Jobs succeeded. |
| `CatalogReady` | False | `WaitingForOperatorMirror` | Build deferred until no image is pending and the current spec is resolved. |
| `CatalogReady` | False | `CatalogBuildRunning` | Job running. |
| `CatalogReady` | False | `CatalogBuildFailed` | Job failed. |

### Generated objects

| Kind | Name | Purpose |
|---|---|---|
| ConfigMap | `<name>-images` | Image state of this ImageSet (gzip JSON, key `images.json.gz`; annotation `mirror.openshift.io/resolved-catalog-digests`). Owned by the MirrorTarget. |
| Job | `catalog-build-<imageset>-<catalog>-<hash>` | One per operator catalog entry. |

---

## MirrorExport

Resolve content into downloadable artifacts without copying images. See
[Consuming the mirror → MirrorExport](../consuming-results.md#mirrorexport-resolve-without-copying).

### Spec

| Field | Type | Description |
|---|---|---|
| `mirror` | [Mirror](#mirror) | **Required.** Same schema as `ImageSet.spec.mirror`. |
| `source.registry` | string | Registry to resolve against instead of upstream (e.g. an already populated mirror). Catalog references in `mirror` must point at it. |
| `source.insecure` | bool | Plain HTTP / skip TLS verification for `source.registry`. |
| `source.authSecret` | string | Credentials used by the export Job for resolution. |
| `source.caBundle` | [CABundleRef](#cabundleref) | CA bundle for the export Job. |
| `destination.registry` | string | **Required.** Registry the destinations and generated resources are computed for. Never contacted. |
| `destination.insecure` | bool | Recorded for consumers. |

### Status

| Field | Type | Description |
|---|---|---|
| `conditions` | []Condition | `Ready`: `ExportBuildRunning`, `Rendered` (True), `ExportBuildFailed`, `RBACFailed`, `ConfigMapFailed`. |
| `observedGeneration` | int64 | |
| `totalImages` | int | Images in the rendered manifest. |
| `artifactsConfigMap` | string | `<name>-artifacts`. |
| `lastRenderedSignature` | string | Hash of the spec the artifacts were rendered from. |

### Generated objects

| Kind | Name | Purpose |
|---|---|---|
| ConfigMap | `<name>-artifacts` | `manifest.json`, `buildspec.json`, `idms.yaml`, `itms.yaml`, `catalogsource-<slug>.yaml`, `clustercatalog-<slug>.yaml`. |
| Job | `export-build-<name>-<hash>` | Renders the artifacts (30 min deadline, 3 retries, 10 min TTL). |
| ServiceAccount, Role, RoleBinding | `<name>-export` | Job identity, may only update the artifacts ConfigMap. |
