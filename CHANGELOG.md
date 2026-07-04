# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **`AdditionalImage.TargetTag` was silently ignored**: `spec.mirror.additionalImages[].targetTag`
  was defined in the API and documented as working, but the collector never applied it —
  the destination always kept the source image's own tag (or whatever tag was baked into
  `targetRepo`). `TargetTag`, when set, now always overrides the destination tag,
  regardless of whether `targetRepo` is also set.
- **CheckExist HTTP 400 on HAProxy/nginx routes**: When the manager pod performs
  drift-check verification against a target registry exposed via an OpenShift Route
  (HAProxy) or Quay's nginx proxy, the `Authorization` Bearer token scope can grow
  beyond the proxy's header-size limit (~8 KB) after authenticating to many
  repositories in a single CheckExist cycle. HAProxy responds with `<BADREQ>` /
  HTTP 400, which previously caused all subsequent checks in the same window to be
  silently skipped. The manager now detects HTTP 400 responses and immediately
  force-refreshes the client cache so subsequent checks use a fresh, narrow-scope
  Bearer token.
- **Catalog rebuild triggered before bundle images are present**: After a catalog's
  upstream digest changed (new operator version), the controller triggered a catalog
  rebuild immediately via `alreadyBuilt=true` shortcut — even though the new bundle
  images were still in `Pending` state. Clusters that consumed the rebuilt catalog
  saw new operator versions available for install while the required images had not
  yet been mirrored to the target registry. The controller now re-gates the rebuild
  on `operatorMirroringComplete` (all operator images `Mirrored` or
  `PermanentlyFailed`) whenever a rebuild is triggered by a signature change or poll
  expiry.

### Added
- **GatewayAPI Exposure**: `spec.expose.type: GatewayAPI` now creates a
  `gateway.networking.k8s.io/v1` HTTPRoute attached to the Gateway referenced by
  `spec.expose.gatewayRef`, following the same create/update/cleanup pattern as
  the existing Route and Ingress exposure types. Requires the Gateway API CRDs
  to be installed and `gatewayRef.name` to be set; the controller reports a
  clear error condition when either is missing.
- **Helm Chart Mirroring**: `spec.mirror.helm.repositories` is now evaluated —
  previously the API types existed but the collector ignored them entirely.
  Charts are downloaded from their repository's `index.yaml`, fully rendered
  using the real `helm.sh/helm/v3` SDK (the same rendering pipeline `helm
  template` uses — not a static `values.yaml` scan, which would miss any
  chart that templates image references dynamically), and every rendered
  manifest is scanned via JSONPath for image references (built-in
  container/init-container paths plus per-chart `imagePaths`).
  `spec.mirror.helm.local` (charts already on disk) remains unimplemented —
  the manager pod has no general host filesystem access. Editable via a new
  REST endpoint (`GET`/`PATCH .../helm`) and a new "Helm" tab on the console
  plugin's ImageSet detail page.
- **Blocked Images**: `spec.mirror.blockedImages` is now evaluated. Images whose
  registry-agnostic repository path matches a blocked name are removed from the
  resolved image set, regardless of whether they originated from a release, an
  operator catalog, or `additionalImages`. Already-mirrored images that become
  blocked are cleaned up from the target registry the same way a removed/narrowed
  ImageSet entry is, when `mirror.openshift.io/cleanup-policy=Delete` is set.
  Editable via a new REST endpoint (`GET`/`PATCH .../blocked-images`) and a new
  "Blocked Images" tab on the console plugin's ImageSet detail page.
- **OpenShift Console Plugin**: A new `ConsolePlugin` controller deploys a dedicated plugin pod (`oc-mirror-plugin`) into the operator namespace and registers it with the OpenShift Console. The plugin provides an integrated multi-page UI directly in the OCP web console — no external URL needed.
  - Pages: MirrorTarget overview, ImageSet detail (with image status), CatalogBrowser, Failed Images
  - Auto-deployed on OpenShift clusters; gracefully skipped on non-OCP Kubernetes
  - Plugin resources (Deployment, Service, ConsolePlugin CR, RBAC) are cleaned up via finalizer when the operator is uninstalled

- **CatalogBrowser — Per-Channel Import/Remove**: Clicking `+ Add` on a single channel now imports only that channel instead of the entire package. Clicking `×` on a channel in the filtered pane removes just that channel. The whole-package import button still exists to add all channels at once. The filtered pane shows each selected channel individually with its own remove button.

- **CatalogBrowser — All Channel Versions in Dropdowns**: The `minVersion`/`maxVersion` dropdowns now show all available operator versions in a channel, not just the channel-head. A `versions` field is added to `ChannelSummary` in the cached ConfigMap (sorted, deduplicated). Cache version bumped `v4 → v5` to force rebuild of existing caches.

- **CatalogBrowser — Version Constraints per Channel**: Version constraint (`minVersion`/`maxVersion`) can now be set per individual channel in addition to per package.

- **MonitoringReconciler**: Manages optional `ServiceMonitor`, `PrometheusRule`, and Grafana dashboard `ConfigMap` resources. Automatically creates/updates these when Prometheus Operator CRDs are present in the cluster.

- **Prometheus Metrics**: Operator controller, manager pods, and worker pods now expose Prometheus metrics (reconcile durations, mirroring progress, error counters).

- **Single-namespace mode**: Operator can be deployed in a single namespace without cluster-wide permissions. `ConsolePlugin` controller uses a watcher pattern to reconcile cluster-scoped `ConsolePlugin` CR without a `ClusterRole`.

- **React/TypeScript Console Plugin UI** (replaces embedded Go-served HTML dashboard):
  - Built with PatternFly v6, React 18, react-router v7
  - Separate `Dockerfile.plugin` and webpack build (`webpack.plugin.js`)
  - Published as `oc-mirror-plugin` container image
  - Dark theme support with PF v6 CSS variables; transparent backgrounds

### Fixed
- **Dark theme**: Removed hardcoded white background in `plugin-styles.css` and `ImageSetDetail` Tabs — plugin now correctly inherits the OCP console dark theme.
- **ConsolePlugin ownership**: Removed illegal namespace-scoped owner reference from the cluster-scoped `ConsolePlugin` CR; cleanup is now handled by a finalizer on the controller pod instead.
- **ConsolePlugin resource cleanup on deletion**: Added `pluginCleanupFinalizer` to ensure Deployment, Service, and ConsolePlugin CR are deleted when the operator is uninstalled.
- **react-router `Link` imports**: Downgraded SDK to 4.21.0 and fixed `Link`/`useParams` imports broken by react-router v7 module layout.
- **Docker build platform**: Added `--platform=$BUILDPLATFORM` to the Node.js build stage in `Dockerfile.plugin` to support cross-compilation from ARM hosts.
- **UI `node_modules` in Docker context**: Excluded `ui/node_modules` from Docker build context via `.dockerignore`.
- **ToolbarItem `align` prop**: Updated deprecated `alignEnd` → `alignRight` for PatternFly v6 compatibility.
- **Stale UIConfiguration CRD**: Removed leftover `UIConfiguration` CRD, ClusterServiceVersion entry, and bundle manifests from OLM bundle.
- **oauth-proxy removal**: Removed leftover oauth-proxy Deployment/Service/Secret from bundle kustomize config.
- **E2E tests**: Replaced hardcoded namespace strings with `operatorNamespace` variable; fixed `IMG_DASHBOARD → IMG_PLUGIN` env var references.

### Changed
- **UIConfiguration CRD removed**: Replaced by the always-on `ConsolePlugin` controller. The CRD is no longer needed — the plugin is automatically enabled on OpenShift clusters.
- **DashboardReconciler merged into ConsolePlugin controller**: Unified reconciliation path for all plugin-related resources.
- **Operator catalog cache version**: Bumped from `v4` to `v5` to force re-population of the operator cache ConfigMaps with the new `versions` field.

---

## [v0.0.15] - 2026-05-02

### Fixed
- **OLM upgrade e2e test**: The "build old bundle" step was reconstructing the bundle from the git tag using `git archive` + `docker build`. Because `config/manager/kustomization.yaml` is committed with dev-registry overrides (`quay.lab.brtrm.dev`), the rebuilt bundle referenced inaccessible images and caused `ImagePullBackOff`. The step now pulls the released bundle image directly from `ghcr.io` instead.
- **Bundle image tag format**: The old bundle pull used the `v`-prefixed git tag (e.g. `v0.0.14`) but ghcr.io bundle images are tagged without the `v` prefix (e.g. `0.0.14`), causing a 404. Fixed to use the version without prefix.
- **Incomplete `spec.relatedImages` in released bundle**: `operator-sdk generate bundle` with `USE_IMAGE_DIGESTS=false` only added the controller container to `spec.relatedImages`, silently dropping manager and worker. This caused `oc-mirror` to mirror only 2 images (bundle + controller) instead of all 3. The release workflow now uses `USE_IMAGE_DIGESTS=true` with real versioned image refs so operator-sdk pulls all three images, resolves them to digest refs, and writes all three into `spec.relatedImages` — matching local `make bundle` behaviour exactly.
- **IDE warnings**: Addressed deprecated API usages, resource leaks, and missing error handling surfaced by static analysis.

### Changed
- **Release workflow streamlined**: Merged the two-step "Regenerate bundle" + "Inject image digest" approach into a single `VERSION="${VERSION}" make manifests bundle` invocation. No placeholder images, no sed substitutions, no manual yq overrides needed.

---

## [v0.0.13] - 2026-05-02

### Added
- **Developer Guide**: New comprehensive documentation for building and deploying the operator in various environments (OpenShift, MicroShift, KinD, standard Kubernetes).
- **Image Status Redesign**: Completely redesigned the image mirroring status tab in the Web UI, providing a more condensed and intuitive view with status badges and registry tooltips.

### Fixed
- **CSV RelatedImages**: Implemented dynamic detection and replacement logic in Kustomize to ensure modular images (manager, worker) are correctly included in the `relatedImages` section of the ClusterServiceVersion with their respective SHA digests.
- **Worker Storage Default**: Refactored worker storage to use node-local `emptyDir` by default for blob buffering. PVC-backed generic ephemeral volumes are now only provisioned if a `storageClassName` is explicitly specified.
- **UI Scroll Restoration**: Fixed a bug where the UI would jump to the top during the 30-second auto-refresh cycle. Scroll position is now preserved across refreshes.
- **UI Font Assets (404s)**: Fixed Red Hat font loading by correcting relative paths and adding a server-side redirect from `/ui` to `/ui/` for consistent path resolution.
- **CRD Spec Clean-up**: Removed `+kubebuilder:default` tag for storage size in the CRD to prevent the Kubernetes API from automatically injecting default values into the spec, while maintaining the 10 GiB fallback in the operator logic.

---

## [0.1.0] - 2026-05-01

### BREAKING CHANGES
- **Container image split**: Operator now requires 3 separate container images instead of a single monolithic image:
  - `oc-mirror-controller` – Kubernetes operator controller
  - `oc-mirror-manager` – Per-MirrorTarget manager pod
  - `oc-mirror-worker` – Worker pods for image mirroring + cleanup subcommand
  - Old `oc-mirror-operator:v0.0.x` image is **deprecated**
- **Installation changes**: Helm charts, OLM bundles, and manual deployments must now reference 3 images instead of 1
- **Deployment procedure**: See [migration guide](docs/migration-v0.0-to-v0.1.md) for upgrade instructions

### Added
- **Modular component architecture**: Separated operator controller, manager orchestration, and worker execution into distinct binaries and container images for improved maintainability and independent scaling
- **Controller-only deployment**: `oc-mirror-controller` can now be deployed independently without embedding manager/worker code
- **Dedicated manager image**: `oc-mirror-manager` focuses solely on worker orchestration and ImageState management
- **Standalone worker pods**: `oc-mirror-worker` binary handles both ephemeral worker pods and cleanup job subcommand
- **Reduced image sizes**: Each modular image is smaller than the monolithic predecessor, enabling faster deployments
- **Separate RBAC roles**: Each component gets its own ServiceAccount and Role for least-privilege access
- **Improved testability**: Modular architecture makes unit testing and local development simpler
- **Helm chart enhancements**: Per-component image configuration and resource limits

### Changed
- **Documentation**: Updated README, user guide, and contributing guide for new 3-component architecture
- **Code organization**: Extracted manager and worker responsibilities into separate main.go entry points while preserving shared libraries in `pkg/mirror/`
- **CI/CD**: Build pipeline now produces 3 optimized images per release
- **Maintainability**: ~30 lines of duplicate code between manager/worker subcommands removed

### Fixed
- Registry connection pooling now isolated per component
- Token scope errors in Quay compatibility fixed (per-component credential isolation)

### Deprecated
- Single-binary deployment (`v0.0.x` and earlier) – **migrate to v0.1.0+** for modular deployment
- Old `oc-mirror-operator` Helm chart tag – use per-component tags in v0.1.0+
- Legacy deployment procedures using single `oc-mirror:vX` image

### Security
- Separated RBAC permissions reduce blast radius if one component is compromised
- Each pod type (controller, manager, worker) has minimal required permissions

### Documentation
- New: [Migration Guide](docs/migration-v0.0-to-v0.1.md) — upgrade from v0.0.x to v0.1.0
- Updated: README Architecture section with component overview
- Updated: User Guide installation and concepts sections
- Updated: Contributing guide build instructions for 3 images

---

## [v0.0.11] - 2026-04-26

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
  MirrorTarget, the client tries plain HTTP first and falls back to HTTPS
  without certificate verification. This avoids a ~60 s TLS handshake timeout
  per request against HTTP-only registries.
- **Catalog build timing logs**: Each step of `BuildFilteredCatalogImage` now
  prints wall-clock time, elapsed seconds, and per-step duration
  (`[HH:MM:SS +Xs ΔXs]`) so CI logs reveal exactly where time is spent.

### Fixed
- **ImageSet polling stuck**: Fixed a bug where the poll clock got stuck after
  resolution errors and a race where ConfigMap deletions went undetected, leaving
  image state stuck on `Pending`.
- **Resource leak**: Closed an HTTP body that was leaked on successful mirror
  HEAD checks; ensured empty state ConfigMap is always created on first
  reconciliation.
- **ConfigMap state persistence**: Fallback linear search for hash-miss in
  `setImageStateLocked`; dirty flag re-set on save failure; worker
  `reportStatus` retries 3× with 2 s backoff.
- **Cache invalidation**: Operator resolution results are now tagged with a
  versioned cache token (`operatorCacheVersion`). Stale entries are invalidated
  on upgrade, preventing incorrect filtering results.
- **Cross-scheme blob transfers**: Replaced `BlobCopy` with explicit
  `BlobGet`+`BlobPut` to avoid indefinite hangs when transferring between OCI
  directory layout and remote registries (regclient server-side mount issue).
- **Per-operation push timeouts**: FBC layer push (5 min), config push (2 min),
  and manifest push (2 min) now have individual context deadlines instead of
  relying on the 20-minute parent context.
- **TLS fallback order**: Insecure registries now try HTTP first, falling back
  to HTTPS-skip-verify. The previous HTTPS-first order wasted ~60 s per
  BlobPut/ManifestPut when the target registry was HTTP-only (e.g. in-cluster
  `registry:5000`), causing 17+ minute stalls for catalog builds.
- **OLM upgrade test**: Corrected coordinator Role name from hardcoded
  `oc-mirror-coordinator` to `<mirrortarget-name>-coordinator`; added operator
  readiness checks after install and upgrade; improved diagnostic dumps.

### Changed
- **Catalog builder refactored to OCI layout**: Source catalogs are now
  downloaded once to a local OCI directory layout via `regclient.ImageCopy`.
  Layer classification and FBC extraction happen via fast local disk I/O,
  eliminating redundant blob downloads.
- **Unit test coverage ≥75%**: All 11 packages now meet the 75% minimum
  coverage threshold (controller 76.6%, catalog 76.0%, client 76.4%,
  imagestate 89.0%, state 86.4%, resources 88.1%, builder 97.1%, release 100%).
- **E2E test stability**: Improved test reliability with better diagnostic
  dumps, cleanup helpers, timeout handling, and label-based test filtering.
- **CI**: Merged the `e2e` and `e2e-olm-upgrade` workflow jobs into a single
  `e2e` job with two phases sharing one KinD cluster.
- **CI**: Removed all Gemini-based CI workflows (triage, code-review, dispatch).
- **CI diagnostics**: Failure dumps now cover both `oc-mirror-system` and
  `oc-mirror-operator` namespaces (pods, CSVs, deployments, roles, logs).

### Documentation
- Updated CHANGELOG, README, user guide, OLM upgrade guide, and API reference
  for all changes in this release.

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

[Unreleased]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.15...HEAD
[v0.0.15]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.13...v0.0.15
[v0.0.13]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.1.0...v0.0.13
[v0.0.11]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.10...v0.0.11
[v0.0.6]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.5...v0.0.6
[v0.0.5]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.4...v0.0.5
[v0.0.4]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.3...v0.0.4
[v0.0.3]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.2...v0.0.3
[v0.0.2]: https://github.com/mariusbertram/oc-mirror-operator/compare/v0.0.1...v0.0.2
[v0.0.1]: https://github.com/mariusbertram/oc-mirror-operator/releases/tag/v0.0.1
