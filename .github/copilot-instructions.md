# Copilot Instructions for oc-mirror-operator

## Commands

```bash
make build                # Build binary (runs generate, manifests, fmt, vet first)
make test                 # Unit tests with envtest (excludes e2e)
make lint                 # golangci-lint (config: .golangci.yml)
make lint-fix             # Auto-fix lint issues
make generate manifests   # Regenerate deepcopy + CRD/RBAC manifests (required after api/ changes)
make run                  # Run controller locally against current kubeconfig
make install              # Install CRDs into cluster
make deploy               # Deploy operator to cluster
make hooks                # Install pre-commit hook (fmt, vet, lint, tests)
```

Single test:
```bash
go test ./internal/controller/ -run TestConditions -v
go test ./pkg/mirror/release/ -run TestResolveReleaseNodes -v
```

Single Ginkgo-style test:
```bash
go test ./internal/controller/ -ginkgo.focus="should reconcile MirrorTarget" -v
```

E2E tests (KinD cluster):
```bash
make test-e2e             # Full e2e with cluster setup/teardown
make test-e2e-cluster     # Build image, load into Kind, run cluster-labeled e2e
make test-integration     # Integration tests without a cluster (labels: integration, release, catalog)
```

E2E environment variables: `SKIP_CLUSTER_SETUP=true`, `SKIP_OPERATOR_DEPLOY=true`, `CERT_MANAGER_INSTALL_SKIP=true`.
E2E Ginkgo labels: `cluster`, `integration`, `release`, `catalog`, `catalog-cluster`, `olm-upgrade`.

## Local Development with MicroShift (minc)

`minc` (MicroShift in Container) runs a lightweight OpenShift cluster inside a container.

### Create Cluster
```bash
# Create and start MicroShift cluster (defaults to podman)
minc create -p podman
```

### Connect with kubectl
The `minc` tool automatically updates your `~/.kube/config` with a `microshift` context.

```bash
kubectl config use-context microshift
kubectl get nodes
```

### Testing with a local Registry
To test image mirroring without an external registry, deploy a local registry in the cluster and expose it:

1. **Deploy Registry**:
   ```bash
   kubectl apply -f config/samples/registry_deploy.yaml
   ```

2. **Expose Registry via Route**:
   `minc` supports OpenShift Routes. Expose the registry to push images from your host:
   ```bash
   # Create a Route to expose the registry service
   oc expose svc/registry --name=registry-external
   
   # Get the external URL (usually registry-external-default.apps.nic.local)
   REGISTRY_URL=$(kubectl get route registry-external -o jsonpath='{.spec.host}')
   ```

3. **Push Test Image**:
   ```bash
   podman build -t $REGISTRY_URL/oc-mirror-operator:test .
   podman push $REGISTRY_URL/oc-mirror-operator:test --tls-verify=false
   ```

4. **Deploy Operator**:
   ```bash
   make deploy IMG=$REGISTRY_URL/oc-mirror-operator:test
   ```

Container tool defaults to **podman** (`CONTAINER_TOOL ?= podman`). Override with `CONTAINER_TOOL=docker` if needed.

## Architecture

The operator uses a three-tier runtime — all tiers share the same binary (`cmd/main.go`) with different subcommands:

| Tier | Code | Responsibility |
|------|------|----------------|
| **Operator Controller** | `internal/controller/` | Two reconcilers: `MirrorTargetReconciler` (manages Manager Deployment, RBAC, cleanup jobs) and `ImageSetReconciler` (manages catalog build Jobs, poll-based re-collection) |
| **Manager Pod** | `pkg/mirror/manager/` | One per MirrorTarget. Coordinates worker pods, owns imagestate ConfigMap, serves IDMS/ITMS via HTTP resource server |
| **Worker Pods** | `cmd/main.go` worker subcommand | Ephemeral. Mirror image batches and report status back to Manager |

Additional entrypoints: `cmd/main.go cleanup` (deletes orphaned images from registry), `cmd/catalog-builder/main.go` (separate binary for OLM FBC filtering, runs in Jobs).

### CRDs (`api/v1alpha1/`)

- **MirrorTarget**: target registry, ImageSet references, concurrency/batch settings, exposure config
- **ImageSet**: content to mirror — releases (Cincinnati graph), operator catalogs (FBC), additional images

### Key Packages

| Package | Purpose |
|---------|---------|
| `pkg/mirror/client/` | OCI registry operations via regclient; blob buffering for large layers |
| `pkg/mirror/collector.go` | Gathers target images from releases, operators, and additional images |
| `pkg/mirror/release/` | Cincinnati graph resolution, version ranges, shortest-path computation |
| `pkg/mirror/catalog/` | OLM FBC parsing, package filtering, transitive dependency resolution |
| `pkg/mirror/imagestate/` | Gzip-compressed ConfigMap state (`<imageset>-images`): source, dest, state, retryCount, origin |
| `pkg/mirror/resources/` | IDMS/ITMS generation |

### Image State Machine

```
Pending → Mirrored (success)
        → Failed (retried up to 10x) → PermanentlyFailed
```

## Hard Invariants

- `MirrorTarget.spec.imageSets` owns the association — `ImageSet` does **not** reference a target
- An `ImageSet` may be referenced by only **one** `MirrorTarget`
- `MirrorTarget` and its `ImageSet` resources must be in the **same namespace**
- Controller tests use `envtest.Environment` (API server + etcd); run `make setup-envtest` when running them directly outside `make test`

## If You Change X, Also Update Y

| Changed | Must also run/update |
|---------|---------------------|
| `api/v1alpha1/*_types.go` | `make generate manifests` (deepcopy + CRD YAML) |
| RBAC markers (`// +kubebuilder:rbac:...`) | `make manifests` |
| CRD types or controller logic | `make test` (controller tests use envtest) |
| `bundle/` contents | **Don't.** Regenerate with `make bundle IMG=<image>` |
| CSV metadata | Edit `config/manifests/bases/oc-mirror-operator.clusterserviceversion.yaml`, then `make bundle` |

## Conventions

### Annotation-Driven Flows
- `mirror.openshift.io/recollect`: one-shot trigger on ImageSet — clears after processing, forces upstream re-resolution
- `mirror.openshift.io/cleanup-policy=Delete`: enables image deletion from registry on ImageSet removal or spec narrowing
- Catalog/release digest annotations on ImageSet status serve as cache-invalidation keys

### Conditions and Status
- All condition updates go through `setCondition()` in `internal/controller/conditions.go`
- `LastTransitionTime` only changes when `Status` flips; `ObservedGeneration` is always set for spec-drift detection

### Reconciliation
- Finalizers for cleanup on deletion (`mirror.openshift.io/cleanup`)
- `controllerutil.CreateOrUpdate()` for idempotent resource management with owner references
- Non-blocking errors: log + requeue with backoff. Blocking errors: update status conditions + return error

### Testing
- Unit tests: table-driven with `t.Run`, co-located `_test.go` files
- E2E: Ginkgo/Gomega with `By()` step annotations
- When creating pods (manager/worker/catalog/cleanup), preserve the existing restricted `securityContext` pattern

## Unimplemented API Fields

These fields exist in `api/v1alpha1/` types but are **not wired up** — do not assume they work:
- `blockedImages`, `helm` (Helm chart mirroring), `samples`
- `platform.graph`, `platform.release` (disk-to-mirror)
- `spec.expose.type: GatewayAPI` (HTTPRoute creation not implemented)
