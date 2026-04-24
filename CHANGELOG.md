# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Heads-only operator filtering (oc-mirror v2 compatible)**: When a package is
  listed without explicit channels or version ranges, only the channel head (latest
  version) of every channel is included — matching `oc-mirror v2` behaviour.
  Previously all versions of all channels were mirrored. To include more than just
  the head, use the new `previousVersions` field.
- **Head+N mode** (`spec.mirror.operators[].packages[].previousVersions`): New
  integer field (default `0`) that includes N older versions behind the channel
  head in heads-only mode. Example: `previousVersions: 2` mirrors the head plus
  the two preceding versions per channel.
- **Catalog packages endpoint**: New Resource Server endpoint
  `GET /resources/{imageset}/catalogs/{catalog}/packages.json` returns all
  packages, channels, and versions from the filtered catalog image. Requires
  `CatalogReady` condition (returns HTTP 409 if not built yet).
- **Upstream catalog packages endpoint**: New Resource Server endpoint
  `GET /resources/{imageset}/catalogs/{catalog}/upstream-packages.json` returns
  the full, unfiltered package list from the upstream source catalog. Useful for
  discovering available operators before configuring the ImageSet filter. No
  `CatalogReady` gate required.
- **TLS fallback for insecure registries**: When `insecure: true` is set on the
  MirrorTarget, the operator first attempts HTTPS without TLS certificate
  verification. If that fails, it falls back to plain HTTP. Previously only
  plain HTTP was used.

### Fixed
- **ConfigMap state persistence**: Fixed an issue where image state could remain
  stuck on `Pending` after successful mirroring. Root causes: fallback linear
  search added for hash-miss in `setImageStateLocked`; dirty flag is re-set on
  save failure to prevent permanent state loss; worker `reportStatus` now retries
  3 times with 2 s backoff.
- **Cache invalidation**: Operator resolution results are now tagged with a
  versioned cache token (`operatorCacheVersion`). Stale cache entries from prior
  operator versions are automatically invalidated on upgrade, preventing
  incorrect filtering results.

### Changed
- **CI**: Merged the `e2e` and `e2e-olm-upgrade` workflow jobs into a single
  `e2e` job with two phases (regular e2e → OLM upgrade) sharing one KinD cluster.
- **CI**: Removed all Gemini-based CI workflows (triage, code-review, dispatch).

### Documentation
- Updated README and user guide with per-MirrorTarget RBAC naming, proxy
  auto-injection details, and multi-MirrorTarget deployment guide.
- Updated all documentation for heads-only filtering, head+N mode,
  `previousVersions` field, TLS fallback, catalog packages endpoint, and
  merged CI pipeline.

---

## [v0.0.6] - 2026-04-23

### Fixed
- **Multi-MirrorTarget support**: RBAC resources (ServiceAccount, Role,
  RoleBinding) now use per-MirrorTarget names (`{name}-coordinator`,
  `{name}-worker`) instead of fixed names. This prevents ownership conflicts
  when multiple MirrorTarget CRs are deployed in the same namespace.
  **Migration**: old fixed-name resources (`oc-mirror-coordinator`,
  `oc-mirror-worker`) are orphaned after upgrade and must be deleted manually.
- **Proxy + Kubernetes API connectivity**: When a proxy is configured,
  `KUBERNETES_SERVICE_HOST` is now overridden to
  `kubernetes.default.svc.cluster.local` in all pod specs (manager, worker,
  catalog-builder). This ensures `client-go` uses the FQDN — which is already
  covered by the auto-injected NO_PROXY rules — instead of the bare ClusterIP
  that bypasses NO_PROXY matching.
- **`image:tag@sha256:digest` references**: Image references that combine a tag
  and a digest (e.g. `registry.example.com/image:v1.2.3@sha256:abc...`) are
  now parsed and handled correctly.
- **Gofmt**: Fixed extraneous blank line in `pkg/mirror/collector_test.go`.

### CI
- OLM upgrade e2e test: now builds a fake v0.0.2 bundle from the git tag and
  pushes both bundles to the local registry before testing the upgrade path.
- Removed stable-4.21 ImageSetConfiguration pipeline test fixture.

---

## [v0.0.5] - 2026-04-23

### Fixed
- **Release workflow**: Bundle manifests are now regenerated (`make bundle`)
  before the bundle image is built. Previously the pre-existing bundle directory
  was used, which could contain stale manifests from prior local development.

---

## [v0.0.4] - 2026-04-23

### Fixed
- **Operator icon**: Restored the operator logo in the base ClusterServiceVersion
  (`config/manifests/bases/oc-mirror-operator.clusterserviceversion.yaml`). The
  icon had been lost during a prior bundle-regeneration step.

### Testing
- Added an OLM upgrade end-to-end test that validates the full upgrade path from
  v0.0.1 to the current version via OLM subscription.

---

## [v0.0.3] - 2026-04-23

### Fixed
- **Bundle CSV**: Regenerated the OLM bundle CSV to include PVC permissions and
  the new feature RBAC rules added in v0.0.2. The v0.0.2 bundle had been built
  from stale manifests.

---

## [v0.0.2] - 2026-04-23

### Added
- **Operator channel/version filtering** (`spec.mirror.operators[].packages`):
  The catalog builder now filters the File-Based Catalog (FBC) to include only
  the packages, channels, and version ranges specified in the ImageSet. Previously
  the full upstream catalog was embedded in the built catalog image.
- **HTTP proxy support** (`spec.proxy`): Worker, manager, and catalog-builder
  pods inherit HTTP/HTTPS proxy settings from the MirrorTarget spec. Cluster-
  internal FQDN suffixes (`localhost`, `127.0.0.1`, `.svc`, `.svc.cluster.local`)
  are automatically added to `NO_PROXY` so in-cluster traffic always bypasses
  the proxy.
- **Custom CA bundle** (`spec.caBundle`): A ConfigMap containing a PEM CA bundle
  can be referenced and will be mounted into all pods with `SSL_CERT_FILE`
  pointing to it. Required for environments with corporate or private CAs.
- **Ephemeral worker PVC** (`spec.workerStorage`): Worker pods can use a
  dynamically-provisioned ephemeral PVC instead of the default 10 GiB `emptyDir`
  for the blob buffer volume. Required for mirroring very large images such as
  AI/ML model images.
- Apache 2.0 LICENSE added to the repository.

### Fixed
- **Catalog gate**: No longer blocks when a catalog build Job already exists;
  now waits for the running job to complete.
- **ResolveCatalog**: Always includes the catalog image itself in the resolved
  image set. Skips bundle extraction for empty package lists.
- **Release pipeline tagging**: Destination tags now use the per-node version
  from the release payload instead of a shared tag, avoiding tag collisions in
  multi-arch builds.
- **CI permissions**: All GitHub Actions workflow jobs use explicit least-
  privilege `permissions:` blocks (deny-all default).
- **Dependencies**: Updated indirect dependencies to resolve known CVEs.
- **E2E test reliability**: Fixed race conditions around manager pod deletion
  and the `recollect` annotation timing.

---

## [v0.0.1] - 2026-04-23

### Added
- **ImageSet CRD**: Declarative image mirror specification supporting OpenShift
  release channels, OLM operator catalogs, additional images, and Helm charts.
- **MirrorTarget CRD**: Target registry configuration binding one or more
  ImageSets. Provides aggregated status counters (total, mirrored, pending,
  failed) across all bound ImageSets.
- **Manager pod**: Per-MirrorTarget stateful manager that orchestrates worker
  pods, resolves upstream digests, tracks per-image state in a ConfigMap, and
  handles retries with exponential backoff.
- **Worker pods**: Parallel image mirroring with configurable concurrency
  (default 20) and batch size (default 10 images per pod).
- **OLM catalog builder**: Kubernetes Job that produces a filtered File-Based
  Catalog (FBC) image from upstream operator catalog sources.
- **Resource server**: HTTP endpoint per MirrorTarget exposing IDMS, ITMS,
  CatalogSource, ClusterCatalog, and release signature manifests for
  disconnected cluster post-mirror configuration.
- **Exposure types**: Route (auto-created on OpenShift), Ingress, GatewayAPI,
  or plain ClusterIP Service for the resource server.
- **Retry and permanent-failure logic**: Per-image retry counter; images that
  exhaust retries are marked `PermanentlyFailed`. The `recollect` annotation
  resets all images for a fresh re-resolution.
- **Cleanup policy**: `mirror.openshift.io/cleanup-policy: Delete` annotation
  triggers image deletion from the target registry when an ImageSet is removed
  from `spec.imageSets`.
- **Pull secret support**: Both `username`/`password` and `.dockerconfigjson`
  Secret formats are supported for the target registry.
- **Security hardening**: Read-only root filesystem, no privilege escalation,
  dropped capabilities, non-root UID/GID, NetworkPolicy restricting pod-to-pod
  traffic, and RBAC with least-privilege service accounts.
- **OLM bundle**: Full OLM bundle (CSV, CRDs, RBAC) for installation via the
  Operator Lifecycle Manager.
- **E2E test suite**: Ginkgo/Gomega end-to-end tests for a KinD cluster
  covering the full mirroring lifecycle.

[Unreleased]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.6...HEAD
[v0.0.6]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.5...v0.0.6
[v0.0.5]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.4...v0.0.5
[v0.0.4]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.3...v0.0.4
[v0.0.3]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.2...v0.0.3
[v0.0.2]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.1...v0.0.2
[v0.0.1]: https://github.com/mariusbertram/oc-mirror-operator/releases/tag/v0.0.1
