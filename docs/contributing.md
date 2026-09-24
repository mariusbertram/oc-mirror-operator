# Contributing

Thanks for contributing. This page covers the process: layout, tests, lint, CI and
releases. Build and deployment mechanics are in the [Developer guide](developer-guide.md).

**Contents**

- [Repository layout](#repository-layout)
- [Development setup](#development-setup)
- [Tests](#tests)
- [E2E tests](#e2e-tests)
- [Linting](#linting)
- [Generated files](#generated-files)
- [CI](#ci)
- [Releases](#releases)
- [Code style](#code-style)
- [Submitting changes](#submitting-changes)

---

## Repository layout

See [Architecture → Repository layout](architecture.md#repository-layout). In short:
`api/` types, `internal/controller/` reconcilers, `pkg/mirror/` the mirroring logic
shared by manager, workers and jobs, `cmd/` one `main` per binary, `ui/` the console
plugin, `config/` kustomize and the CSV base, `test/e2e/` Ginkgo suites, `docs/` this
documentation.

## Development setup

```bash
git clone https://github.com/mariusbertram/oc-mirror-operator.git && cd oc-mirror-operator
go mod download
make controller-gen kustomize golangci-lint setup-envtest    # pinned tools into bin/
make hooks                                                   # pre-commit: fmt, vet, lint, tests
```

`AGENTS.md`/`CLAUDE.md` hold the condensed conventions that AI assistants and humans
alike are expected to follow (invariants, "if you change X also update Y", coverage
expectations).

## Tests

```bash
make test                                   # everything except e2e; writes cover.out
go test ./pkg/mirror/release/ -run TestResolveReleaseNodes -v
go test ./internal/controller/ -ginkgo.focus="should reconcile MirrorTarget" -v
go tool cover -html=cover.out
```

- Unit tests are co-located (`_test.go`), table-driven with `t.Run`.
- Controller tests use envtest (a real API server + etcd downloaded by `make setup-envtest`).
- Manager tests use the controller-runtime fake client and `httptest` registries.
- Target: **≥ 90 % per package** of hand-written Go code. `cmd/*` (thin wiring) and
  generated deepcopy code are excluded. Document genuinely unreachable branches instead
  of forcing fault injection.

## E2E tests

Ginkgo suites in `test/e2e/`, labelled `cluster`, `integration`, `release`, `catalog`,
`catalog-cluster`, `olm-upgrade`.

```bash
make test-integration          # no cluster needed (integration, release, catalog labels)
make test-e2e-cluster          # creates a Kind cluster, builds and loads the image, runs the "cluster" label
make test-e2e                  # full suite incl. OLM upgrade phase
```

Useful variables: `KIND_CLUSTER`, `KIND_PROVIDER` (`docker`/`podman`),
`SKIP_CLUSTER_SETUP=true`, `SKIP_OPERATOR_DEPLOY=true`, `CERT_MANAGER_INSTALL_SKIP=true`,
`TEST_CATALOG_IMAGE`. The operator is deployed once in `BeforeSuite`; each test creates
and removes its own resources.

## Linting

```bash
make lint            # golangci-lint v2, config in .golangci.yml, same version as CI
make lint-fix
gofmt -l .           # must print nothing
npm --prefix ui run lint && npx --prefix ui tsc --noEmit -p ui/tsconfig.json
```

CI fails on any golangci-lint finding, including `prealloc` and `lll` in test files.

## Generated files

| Changed | Regenerate |
|---|---|
| `api/v1alpha1/*_types.go` | `make generate manifests` (deepcopy, CRDs) |
| `// +kubebuilder:rbac` markers | `make manifests` |
| CSV metadata (`config/manifests/bases/…clusterserviceversion.yaml`) | `make bundle` |
| `bundle/`, `catalog/` | never by hand |

## CI

`.github/workflows/`:

| Workflow | Trigger | Jobs |
|---|---|---|
| `ci.yml` | push, PR | unit tests → build component images (artifact) → e2e on Kind (regular + OLM upgrade) → console plugin smoke test → multi-arch build check → bundle build check → grype |
| `lint.yml` | push, PR | golangci-lint + gofmt ("Run on Ubuntu"), UI lint + type-check ("Run on UI") |
| `dependency-review.yml` | PR | dependency review |
| `release.yml` | tag `v*` | multi-arch images → GHCR, bundle, GitHub release, PR against `brtrm-dev-catalog` |

Every job declares least-privilege permissions; images are built once and reused via
artifacts.

## Releases

1. Update `config/manifests/bases/oc-mirror.clusterserviceversion.yaml` (version,
   `replaces`, permissions) and `CHANGELOG.md`.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. `release.yml` builds and pushes `ghcr.io/mariusbertram/oc-mirror-operator-{controller,manager,worker,plugin,bundle}:vX.Y.Z`,
   creates the GitHub release and opens the catalog PR.

## Code style

- `gofmt`, standard Go idioms, errors wrapped with `fmt.Errorf("context: %w", err)`.
- Small named helpers instead of long reconcile bodies; comment the non-obvious *why*,
  not the *what*.
- Structured logging via controller-runtime in the controller; `oclog` in manager and
  workers.
- All condition updates go through `setCondition()`; `observedGeneration` is always set.
- New pods keep the restricted security context of the existing ones.
- Conventional commit subjects (`fix: …`, `feat: …`, `docs: …`).

## Submitting changes

1. Branch from `main`, make the change with tests.
2. `make test lint` and, for UI changes, the UI checks above.
3. Open a PR against `main`; CI must be green. Stacked PRs are welcome — say so in the
   description and base each PR on the previous branch.
4. Docs live in `docs/`; update the page that documents the behaviour you changed in the
   same PR.
