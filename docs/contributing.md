# Contributing Guide

Thank you for contributing to `oc-mirror-operator`! This document describes
the project layout, development workflow, CI pipeline, and how to run tests.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Repository Layout](#repository-layout)
3. [Development Setup](#development-setup)
4. [Building](#building)
5. [Unit Tests](#unit-tests)
6. [E2E Tests](#e2e-tests)
7. [Linting](#linting)
8. [Generating Manifests](#generating-manifests)
9. [CI Pipeline](#ci-pipeline)
10. [Release Process](#release-process)
11. [Code Style](#code-style)
12. [Submitting Changes](#submitting-changes)

---

## Prerequisites

| Tool | Version | Purpose |
|---|---|---|
| Go | ≥ 1.24 (see `go.mod`) | Build and test |
| Docker or Podman | any recent | Build container images |
| KinD | ≥ 0.25 | Local E2E cluster |
| kubectl | ≥ 1.29 | Cluster interaction |
| operator-sdk | ≥ 1.37 | Bundle/CSV generation |
| controller-gen | auto-installed | CRD/RBAC code generation |
| kustomize | auto-installed | Config building |

Install the pinned versions of controller-gen and kustomize used by the project:

```bash
make controller-gen kustomize
```

---

## Repository Layout

```
.
├── api/v1alpha1/          # CRD types (MirrorTarget, ImageSet)
├── cmd/                   # main.go entry points (operator, manager, worker, resource-api, cleanup)
│   └── catalog-builder/   # catalog-builder binary
├── config/                # Kustomize config (CRDs, RBAC, manifests)
│   ├── crd/               # Generated CRD manifests
│   ├── rbac/              # RBAC for the operator itself
│   ├── manager/           # Manager Deployment
│   ├── samples/           # Example CR YAML files
│   └── manifests/bases/   # Base CSV for OLM bundle
├── bundle/                # Generated OLM bundle (do not edit manually)
├── docs/                  # Documentation
├── internal/controller/   # Reconciliation logic (MirrorTarget, ImageSet)
├── pkg/
│   ├── mirror/
│   │   ├── manager/       # Manager pod orchestration logic
│   │   ├── catalog/       # OLM catalog filtering and FBC building
│   │   ├── client/        # OCI registry client (blob upload, image copy)
│   │   ├── imagestate/    # Gzip-compressed ConfigMap state management
│   │   ├── resources/     # IDMS/ITMS/CatalogSource generation helpers
│   │   └── release/       # Cincinnati graph resolution, version ranges
│   ├── resourceapi/       # Standalone Resource API server (REST + Web UI)
│   │   └── ui/            # Embedded Web UI Dashboard (HTML/CSS/JS)
│   └── ...
├── test/e2e/              # End-to-end tests (Ginkgo)
├── hack/                  # Helper scripts
├── Makefile               # Build targets
└── .github/workflows/     # CI/CD pipelines
```

---

## Development Setup

```bash
# Clone the repository
git clone https://github.com/mariusbertram/oc-mirror-operator.git
cd oc-mirror-operator

# Install Go dependencies
go mod tidy

# Install code-generation tools
make controller-gen kustomize

# Install the pre-commit hook (runs fmt, vet, lint, and tests before each commit)
make hooks
```

---

## Building

The oc-mirror operator uses a modular architecture with three separate container images. All binaries can be built together:

```bash
# Build all three binaries (controller, manager, worker)
make build

# Build individual binaries
make build/controller    # Build controller only
make build/manager       # Build manager only
make build/worker        # Build worker only
```

### Container Images

**Build all three images:**

```bash
# Using podman (default)
make docker-build-all IMG_BASE=<your-registry>/oc-mirror

# Using docker
make docker-build-all IMG_BASE=<your-registry>/oc-mirror CONTAINER_TOOL=docker
```

This creates three images with `IMG_BASE` as prefix:
- `<your-registry>/oc-mirror-controller:dev`
- `<your-registry>/oc-mirror-manager:dev`
- `<your-registry>/oc-mirror-worker:dev`

**Or build individual images:**

```bash
make docker-build-controller IMG_BASE=<your-registry>/oc-mirror
make docker-build-manager    IMG_BASE=<your-registry>/oc-mirror
make docker-build-worker     IMG_BASE=<your-registry>/oc-mirror
```

**Push images:**

```bash
make docker-push-all IMG_BASE=<your-registry>/oc-mirror

# Or push individually
make docker-push-controller IMG_BASE=<your-registry>/oc-mirror
make docker-push-manager    IMG_BASE=<your-registry>/oc-mirror
make docker-push-worker     IMG_BASE=<your-registry>/oc-mirror
```

**Multi-arch images (linux/amd64 + linux/arm64):**

```bash
make docker-buildx-all IMG_BASE=<your-registry>/oc-mirror
```

---

## Modular Architecture

Each component is a separate binary with distinct responsibilities:

| Component | Binary Location | Dockerfile | Environment |
|-----------|-----------------|-----------|-------------|
| **Controller** | `cmd/controller/main.go` | `Dockerfile.controller` | Kubernetes operator (watches CRs) |
| **Manager** | `cmd/manager/main.go` | `Dockerfile.manager` | Per-MirrorTarget Pod (orchestrates workers) |
| **Worker** | `cmd/worker/main.go` | `Dockerfile.worker` | Ephemeral Pods + cleanup Job (mirrors images) |

All three share common libraries in `pkg/mirror/` to avoid code duplication and maintain consistency across components.

## Unit Tests

```bash
make test
```

This runs all unit tests and writes a coverage report to `cover.out`. To view
the HTML coverage report:

```bash
go tool cover -html=cover.out
```

**Adding tests**: Unit tests live alongside the package they test
(`_test.go` suffix). Table-driven tests with `t.Run` subtest names are
preferred. Tests must not depend on a running cluster.

---

## E2E Tests

End-to-end tests require a KinD cluster with the operator deployed.

### Quick start

```bash
# 1. Create a KinD cluster with a local registry
./hack/kind-with-registry.sh

# 2. Build and load the operator image into the cluster
make docker-build IMG=example.com/oc-mirror:v0.0.1
kind load docker-image example.com/oc-mirror:v0.0.1

# 3. Deploy the operator via kustomize
kubectl apply -k config/default

# 4. Run the e2e suite
make test-e2e
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `IMG` | `example.com/oc-mirror:v0.0.1` | Operator image used in the Deployment. |
| `KIND_PROVIDER` | `kind` | Container runtime for KinD (`docker` or `podman`). |
| `TEST_CATALOG_IMAGE` | see e2e code | Upstream catalog image used in catalog tests. |

### Test structure

The e2e suite is in `test/e2e/` and uses Ginkgo with `BeforeSuite`/`AfterSuite`
hooks. The operator is deployed **once** in `BeforeSuite`; individual tests
create their own MirrorTarget and ImageSet resources and clean up after
themselves. CRDs are never deleted between test runs to avoid race conditions
with the API server.

There are two test categories:

- **In-memory catalog tests** — pure Go, no cluster needed (`make test` includes
  these via build tag).
- **Cluster tests** — require a KinD cluster with the operator deployed.
  - *Regular e2e* (`cluster` label): mirror lifecycle, Resource API Deployment/Service
    verification, resource ConfigMap persistence, catalog builds.
  - *Catalog-cluster tests* (`catalog-cluster` label): require a reachable
    upstream catalog image, verify catalog resources in ConfigMaps.
  - *OLM upgrade tests* (`olm-upgrade` label): validate the full OLM upgrade
    path. Run as Phase 2 of the merged CI e2e job.

---

## Linting

```bash
make lint
```

This runs `golangci-lint` with the config in `.golangci.yml`. The same version
is used in CI (`v2.11.4`). Fix auto-fixable issues with:

```bash
golangci-lint run --fix
```

Always run `gofmt -w ./...` before committing to avoid CI failures on whitespace
or formatting issues.

---

## Generating Manifests

After changing CRD types or adding RBAC markers, regenerate manifests:

```bash
# Regenerate deepcopy methods and CRD manifests
make generate manifests

# Regenerate the OLM bundle (CSV, CRDs, RBAC)
make bundle IMG=<registry>/oc-mirror:<version>
```

> **Do not edit files in `bundle/` manually.** They are overwritten by
> `make bundle`. Instead, edit the base CSV at
> `config/manifests/bases/oc-mirror-operator.clusterserviceversion.yaml`.

---

## CI Pipeline

The project uses GitHub Actions. Workflows are in `.github/workflows/`.

| Workflow | Trigger | Jobs |
|---|---|---|
| `ci.yml` | push / pull_request | unit tests → build image → e2e (KinD, two phases: regular + OLM upgrade) → multi-arch check → bundle build check |
| `lint.yml` | push / pull_request | golangci-lint + gofmt |
| `release.yml` | push tag `v*` | build multi-arch image → push to GHCR → regenerate bundle → build & push bundle image → create GitHub release |

### CI principles
- **Least-privilege permissions**: Every job specifies only the permissions it
  needs. The default is `permissions: {}` (deny all).
- **Build once, reuse**: The operator image is built once and exported as a
  workflow artifact, then loaded into the KinD cluster by the e2e job. This
  avoids redundant builds.
- **Node.js 24 compatible actions**: All action versions are pinned to versions
  compatible with the GitHub Actions Node.js 24 runtime.

---

## Release Process

1. Ensure all changes are on `main` and tests pass.
2. Update `config/manifests/bases/oc-mirror-operator.clusterserviceversion.yaml`
   with the new version and any new features/permissions.
3. Create and push a git tag:
   ```bash
   git tag v0.0.X
   git push origin v0.0.X
   ```
4. The `release.yml` workflow automatically:
   - Builds a multi-arch image and pushes it to GHCR.
   - Runs `make bundle IMG=ghcr.io/mariusbertram/oc-mirror-operator:v0.0.X` to
     regenerate the OLM bundle from the latest manifests.
   - Builds and pushes the bundle image.
   - Creates a GitHub Release.
5. Update `CHANGELOG.md` with the new version section.

---

## Code Style

- Follow standard Go idioms and `gofmt` formatting.
- Keep functions focused — split large reconcile loops into named helpers.
- Prefer named return variables for complex error-return functions.
- Errors should be wrapped with `fmt.Errorf("context: %w", err)`.
- Log with structured fields via `ctrl.Log` (zap-based). Use `Info` for normal
  flow, `Error` only for actual errors.
- Do not add comments that just restate the code; comment only non-obvious logic.
- Table-driven tests with meaningful subtest names.

---

## Submitting Changes

1. Fork the repository and create a feature branch:
   ```bash
   git checkout -b fix/my-bug-fix
   ```
2. Make your changes, add/update tests.
3. Run `make test lint` locally — ensure everything passes.
4. Run `gofmt -w ./...` to fix formatting.
5. Commit with a conventional commit message:
   ```
   fix: resolve worker SA name collision for multi-MirrorTarget deployments
   ```
6. Push and open a Pull Request against `main`.
7. Address any CI failures or review comments.
