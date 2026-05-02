# pkg/mirror/ as Shared Library

This directory contains a set of shared libraries used by oc-mirror-operator's three-tier architecture:

- **Controller** (`internal/controller/`) — Reconciliation of MirrorTarget and ImageSet CRDs
- **Manager Pod** (`pkg/mirror/manager/`) — Per-MirrorTarget coordination and image state management
- **Worker Pod** (`cmd/worker/`) — Parallel image mirroring
- **Catalog-Builder Job** (`cmd/catalog-builder/`) — OLM FBC filtering

---

## Tier 1: Public APIs (Stable & Reusable)

### `client.MirrorClient` (427 LOC)
Wraps `regclient` to provide OCI registry operations with oc-mirror-operator-specific configuration:
- Blob buffering for large layers
- Insecure/HTTP registry support
- Configurable blob size limits per destination host

**Usage**: All components that need registry access
**Stability**: HIGH — Actively used by 8+ modules
**Examples**:
```go
mc := mirrorclient.NewMirrorClient(nil, authConfigPath)
exists, err := mc.CheckExist(ctx, destination)
```

### `client.ClientCache` (80 LOC, NEW)
Connection pooling for `MirrorClient` instances to prevent auth token scope accumulation and reduce connection overhead.

**Usage**: Components that make repeated registry calls
**Stability**: HIGH — Simple pooling with automatic refresh
**Examples**:
```go
cache := mirrorclient.NewClientCache()
client, _ := cache.GetOrCreate(nil, authConfigPath)
exists, err := client.CheckExist(ctx, destination)
// Auto-refreshes after 5 minutes to prevent token scope creep
```

### `collector.Collector` (275 LOC)
Main orchestrator for gathering target images from:
- Cincinnati release graphs (versions and component images)
- OLM FBC catalogs (packages and transitive dependencies)
- Additional images (explicit manifests)

Returns a flat list of `TargetImage` for mirroring.

**Usage**: Initial image gathering during reconciliation
**Stability**: HIGH — Well-scoped single responsibility
**Examples**:
```go
collector := mirror.NewCollector(mc)
targets, err := collector.Collect(ctx, releaseChannels, catalogs, additionalImages)
```

### `imagestate.ImageState` (345 LOC)
Per-image state tracker stored as gzip-compressed ConfigMap entries:
- Source and destination image references
- State machine: Pending → Mirrored / Failed
- Retry counter (up to 10 attempts before "Permanently Failed")
- Error messages and metadata

**Usage**: Persistent state across manager restarts
**Stability**: HIGH — Core data structure
**Examples**:
```go
state := imagestate.ImageState{}
state[destRef] = &imagestate.ImageEntry{
    Source: "registry.example.com/app:v1.0",
    State:  "Pending",
}
```

### `release.ReleaseResolver` (645 LOC)
Resolves OpenShift release graphs and version ranges via Cincinnati:
- Version filtering (latest, minor range, patch range)
- Component image extraction from release payloads
- Shortest-path computation for release chains

**Usage**: Translating user-specified release constraints into concrete images
**Stability**: HIGH — Stable Cincinnati integration
**Examples**:
```go
resolver := release.New(mc)
nodes, err := resolver.Resolve(ctx, channels, minVersion, maxVersion)
```

### `catalog.CatalogResolver` (1488 LOC)
OLM File-Based Catalog (FBC) parsing and filtering:
- Extracts operator packages from FBC bundles
- Transitive dependency resolution
- Package filtering by spec constraints
- Bundle image resolution

**Usage**: Operator catalog mirroring
**Stability**: HIGH — Core OLM integration
**Examples**:
```go
resolver := catalog.New(mc)
packages, err := resolver.Resolve(ctx, catalogImages, packageFilters)
```

### `resources.*` (542 LOC)
Kubernetes resource generators for downstream consumption:
- `GenerateIDMS()` — ImageDigestMirrorSet
- `GenerateITMS()` — ImageTagMirrorSet
- `GenerateCatalogSource()` — OLM CatalogSource with mirror coordinates

**Usage**: Creating mirror configuration resources after image collection
**Stability**: HIGH — Stable Kubernetes API usage
**Examples**:
```go
idms := resources.GenerateIDMS(namespace, imageMappings)
```

---

## Tier 2: Internal APIs (Limited Scope)

### `manager.*` (1900+ LOC)
Pod-specific orchestration for a single MirrorTarget:
- Worker pod lifecycle management
- Image state reconciliation
- Drift detection (verify images still exist in registry)
- Status API for worker callbacks
- Idempotent resource creation (Deployments, ConfigMaps, etc.)

**Usage**: **Internal only** — instantiated by `cmd/main.go manager` subcommand
**Scope**: Single MirrorTarget coordination

### `catalog/builder.JobBuilder` (491 LOC)
Generates Kubernetes Job manifests for OLM FBC filtering:
- Filtered FBC overlay creation
- Job configuration and environment setup

**Usage**: **Internal only** — instantiated by Controller during ImageSet reconciliation
**Scope**: OLM catalog build job creation

### `state.StateManager` (165 LOC)
Metadata I/O to registry (experimental):
- Stores operator catalog digests in registry
- Implements cache invalidation keys

**Usage**: **Internal** — experimental; used by catalog resolution
**Stability**: EXPERIMENTAL — May change

### `worker.WorkerPool` (108 LOC)
Parallel goroutine pool for mirroring tasks:
- Task distribution across worker goroutines
- Error collection and retry handling

**Usage**: **Internal only** — instantiated by `cmd/worker` subcommand
**Scope**: Single worker pod's image mirroring

---

## Tier 3: Separate Binaries (Not Shared)

### `cmd/catalog-builder/main.go`
Separate binary that runs in Jobs for OLM catalog filtering:
- Uses `pkg/mirror/catalog.CatalogResolver` (shared lib)
- Uses `pkg/mirror/client.MirrorClient` (shared lib)
- Self-contained Job logic not shared with manager

### `cmd/worker/main.go`
Separate binary that runs in Pods for parallel mirroring:
- Uses `pkg/mirror/worker.WorkerPool` (shared lib)
- Uses `pkg/mirror/client.MirrorClient` (shared lib)
- Pulls batches from manager status API

### `cmd/cleanup/main.go`
One-shot binary that cleans up orphaned images:
- Uses `pkg/mirror/client.MirrorClient` (shared lib)
- Deletes images when ImageSet narrows or is deleted

---

## Architecture Notes

### Connection Pooling Strategy
- **ClientCache** in `pkg/mirror/client/cache.go` provides automatic client refresh every 5 minutes
- Prevents auth token scope accumulation (Quay's nginx rejects tokens > ~8 KB)
- Used by Manager for repeated CheckExist calls during drift detection
- Keys by authConfigPath, allowing per-auth pooling

### State Machine
Images transition through states: **Pending** → **{Mirrored, Failed}** → **{Pending (retry), PermanentlyFailed}**
- ConfigMap-based persistence in `ImageState`
- Up to 10 retry attempts before marking permanently failed
- Drift detection re-checks images periodically to catch transient failures

### Dependency Hierarchy (Clean)
```
Core:         client.MirrorClient
              ├─ state.StateManager
              ├─ release.ReleaseResolver
              ├─ catalog.CatalogResolver
              └─ worker.WorkerPool

Orchestration: collector.Collector (uses all above)
              imagestate.ImageState (independent)
              resources.* (uses imagestate)

Pod-specific:  manager.* (uses all)
              catalog/builder.* (uses catalog, resources)
```

No circular imports; clear Tier 1→4 layering.

---

## Usage Guidelines

### For New Consumers
If you need to mirror images from a third system:
1. Use `collector.Collector` to gather target images
2. Use `client.MirrorClient` for registry operations
3. Use `imagestate.ImageState` to track progress
4. Use `resources.*` to generate Kubernetes configuration

### For Testing
All components have test doubles:
- Fake `MirrorClient` in tests (check `*_test.go` files)
- Table-driven tests for isolated logic
- Integration tests with `envtest` for Kubernetes APIs

### For Contributing
When adding new functionality:
- Keep it in the appropriate tier (ask: "Is this reusable outside manager?")
- Add tests in co-located `_test.go` files
- Update this document if introducing new shared libraries
- Avoid importing `manager` from other tiers

---

## Recent Improvements (Phase 5)

### ClientCache Implementation
- Replaced 5+ manual `NewMirrorClient()` calls with pooled caching
- Automatic refresh every 5 minutes prevents token scope creep
- ~30 LOC of duplication removed from manager.go drift detection logic
- Tests: All 166 manager tests passing

### Code Deduplication
- Drift detection CheckExist blocks unified to use shared ClientCache
- Manual refresh counter (checkCount%20) replaced with cache-driven refresh
- Total savings: ~40 LOC of duplication eliminated

**Baseline**: ~100 LOC avoidable duplication  
**After Phase 5**: ~60 LOC removed, architecture improved

---

**Last Updated**: Phase 5 (2026-01-21)
