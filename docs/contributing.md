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
- [Supply-chain evidence](#supply-chain-evidence)
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
`catalog-cluster`.

```bash
make test-integration          # no cluster needed (integration, release, catalog labels)
make test-e2e-cluster          # creates a Kind cluster, builds and loads the image, runs the "cluster" label
make test-e2e                  # full suite
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
| `ci.yml` | push, PR | unit tests; component image build + scans → e2e on Kind; five-version plugin scan matrix; console plugin smoke matrix; multi-arch build check; bundle validation + image build check |
| `lint.yml` | push, PR | golangci-lint + gofmt ("Run on Ubuntu"), UI lint + type-check ("Run on UI") |
| `dependency-review.yml` | PR | dependency review |
| `release.yml` | tag `v*` | tests → parallel multi-arch component/plugin builds + platform scans/attestations → bundle + scans/attestations → GitHub release and catalog PR |

Every job declares least-privilege permissions. The e2e job reuses the component
images and default 4.19 plugin exported by the `build` job; the independent plugin
scan matrix builds each supported console profile without changing that tar-artifact
contract. CI image scans cover `linux/amd64` and report findings without blocking on
severity. Scanner/tool failures still fail their jobs.

## Releases

1. Update `config/manifests/bases/oc-mirror.clusterserviceversion.yaml` (version,
   `replaces`, permissions) and `CHANGELOG.md`.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. `release.yml` builds and pushes controller, manager and worker images tagged
   `vX.Y.Z`, five plugin images tagged `vX.Y.Z-ocp4.18` through `vX.Y.Z-ocp4.22`,
   and the bundle tagged `X.Y.Z`, under `ghcr.io/mariusbertram/oc-mirror-operator-*`.
   It creates the GitHub release and opens the catalog PR after scanning succeeds.

Controller, manager and worker use parallel matrix jobs; the five plugin profiles
also build and scan in parallel. Each component records its manifest-index digest
in a distinct artifact only after both platform scans and attestations succeed.
The `build-operator` aggregation job preserves the downstream component-digest,
version and tag outputs without relying on last-writer-wins matrix job outputs.
The bundle waits for all component and plugin jobs.

## Supply-chain evidence

`.github/actions/sbom-scan-attest` generates an SPDX JSON SBOM with Syft and scans
the same image subject with Grype, producing a human-readable log, reusable JSON
and SARIF. Release scans gate on Critical findings after applying the reviewed
`vex/oc-mirror-operator.openvex.json` statements to component and plugin images;
the workflow does not generate new VEX justifications automatically. Keep VEX
statements narrowly scoped to the affected package/version and supported by
evidence, rather than adding broad ignores.
Downstream scanners must retrieve and explicitly apply the attested VEX;
publishing an attestation does not itself make a scanner consume it automatically.

For every release component, plugin variant and bundle, the action resolves the
`linux/amd64` and `linux/arm64` child digests from the published manifest index and
scans each separately. Keyless cosign SPDX SBOM and, where supplied, OpenVEX
attestations attach to those exact child digests, not the index. Consumers looking
up attestations must resolve the appropriate platform digest first; one index-level
scan must not be interpreted as coverage of both architectures. Images are pushed
before scanning so registry attestations can be written; a failed release scan
blocks downstream bundle/release publication but does not remove already-pushed
image tags.

Each `sbom-scan-*` workflow artifact retains the SPDX SBOM, Grype JSON/SARIF,
scanned-subject metadata, resolved manifest index (for release scans), and applied
VEX for 30 days. Artifacts are uploaded even after a scan failure, preserving
whatever evidence was produced. SARIF is also submitted to GitHub code scanning;
use workflow artifacts for tag-triggered releases, whose findings are not shown
like default-branch/PR results in the Security tab.

These are full final-image scans, not scans limited to the plugin Go binary: Syft
and Grype can discover recognizable OS packages and Go module/build metadata.
However, the final plugin image contains optimized webpack assets, not the UI's
`node_modules` or lockfiles, so bundled JavaScript dependencies may not be
identifiable by image catalogers. A clean image scan is not proof that every
React/PatternFly/npm dependency was examined; PR dependency review remains a
separate source-level check. Local scanner results can also differ from CI with
different scanner versions or vulnerability-database snapshots.

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
