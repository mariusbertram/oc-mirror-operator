# Pipeline Analysis — oc-mirror-operator

**Scope:** `.github/workflows/ci.yml`, `release.yml`, `lint.yml`,
`dependency-review.yml`, the `sbom-scan-attest` composite action, the
`Makefile` bundle/catalog targets and the release → catalog distribution
flow.

**Context:** The release pipeline publishes images to `ghcr.io`, creates a
GitHub Release, and opens a PR — but **only** to the private
`brtrm-dev-catalog`. The two catalogs that actually matter for operator
discovery are missing from the automation: **operatorhub.io**
(`k8s-operatorhub/community-operators`) and the **Red Hat community
catalog** (`redhat-openshift-ecosystem/community-operators-prod`, which
feeds OperatorHub on OpenShift).

---

## What already works well

- **Least-privilege permissions** (`permissions: {}` + per-job grants) on all
  four workflows — exemplary.
- **Supply-chain hygiene:** Grype scans with SARIF upload, Syft SBOMs,
  cosign attestations, digest-pinned bundle references, VEX documents.
- **Deep test pyramid:** unit → bundle validation (operatorframework +
  operatorhubv2 suites) → multi-arch build check → Kind e2e → real-console
  plugin smoke test across 5 OpenShift versions with screenshots.
- **Retry logic and diagnostics:** Kind creation retried 3×, thorough
  failure dumps (pods, events, logs, node images).

---

## Findings

### PIPE-1 — No automated PR to the operatorhub.io community catalog

**Severity: high (distribution gap)**

The release only registers the bundle in `brtrm-dev-catalog`. There is no
job that submits to `k8s-operatorhub/community-operators`, so the operator
is invisible on operatorhub.io. The submission flow is standardized:
build a PR against the catalog repo from the released bundle (digest-pinned,
multi-arch). This can be fully automated in `release.yml` after
`github-release` succeeds, gated on a stable version (semver without
pre-release), using a `OPERATORHUB_PAT`/GitHub App token and the same
clone-edit-PR pattern already used for `brtrm-dev-catalog`.
Bundle validation already runs the `operatorhubv2` suite in CI, so the
bundle should pass the catalog's CI checks.

### PIPE-2 — No automated PR to the Red Hat community-operators catalog

**Severity: high (distribution gap — the primary audience is OpenShift)**

For an operator "primarily for OpenShift", the
`redhat-openshift-ecosystem/community-operators-prod` catalog is the one
that reaches OpenShift admins via OperatorHub out of the box. Submission is
a PR with the bundle under
`community-operators/<package>/<version>/`. The workflow can open this PR
automatically (draft PR, manual merge — Red Hat reviewers still gate it),
falling back to a manual reminder comment if automation is not desired.

### PIPE-3 — No dependabot.yml, and known vulnerabilities on main

**Severity: medium-high**

GitHub reports 3 moderate Dependabot alerts on the default branch, and
there is **no `.github/dependabot.yml`**, so no automated update PRs are
ever opened. Add config for the four ecosystems in play:
`gomod` (`/`), `npm` (`/ui`), `github-actions` (`/.github/workflows`),
`docker` (`/Dockerfile*`). With `open-pull-requests-limit` and grouped
updates this also stops the CI actions from silently drifting (the repo
already pins `actions/checkout@v6` etc. via tags only, not SHA).

### PIPE-4 — CI runs are not deduplicated and not path-filtered

**Severity: medium (runner time, feedback latency)**

- No `concurrency:` group with `cancel-in-progress` — a push to a PR branch
  triggers both `push` and `pull_request` CI runs (the `on.push` filter
  includes `main`, `feature/**`, `fix/**`, which overlap with open PRs).
- No path filters: a README change runs the full 45-minute e2e suite; a
  `ui/`-only change runs all Go jobs. `dorny/paths-filter` (or per-job
  `paths:` in separate workflows) would cut most redundant runs.
- No explicit `timeout-minutes` on jobs — a hung e2e burns the runner for
  the default 360 minutes instead of the expected ~45.

### PIPE-5 — No caching for npm/Go builds in matrix jobs

**Severity: medium (speed)**

`setup-go` caches Go modules, but:
- `scan-plugin` (5 console versions) and `plugin-smoke-test` (5 versions)
  each run `npm ci` + `install:console-profile` + a full plugin build from
  scratch — 10 identical `node_modules` installs per CI run.
- The `build` job compiles Go binaries inside Docker without layer/cache
  reuse; `docker/build-push-action` supports `cache-from`/`cache-to` (GHA
  cache) which the CI build jobs don't use.
- Go build caching across `build`/`build-multiarch-check`/`build-bundle-check`
  could be shared via `actions/cache` with `~/.cache/go-build`.

### PIPE-6 — Release pipeline skips lint and has no dispatch/manual safety

**Severity: medium**

- `release.yml` runs `make test` but **not** `make lint` and not the bundle
  validate suite — a tag pushed from a branch that never saw CI (or after a
  lint regression on main) ships unvalidated. Lint is fast; include it in
  the release `test` job or make it a `needs:` dependency.
- No `workflow_dispatch` trigger and no pre-release gate: any `v*` tag
  publishes immediately. For a catalog-submission flow (PIPE-1/2) an
  accidental tag means automated PRs against public catalogs. A
  `workflow_dispatch` with a version input, or at minimum an `if` on
  semver format, reduces blast radius.

### PIPE-7 — CATALOG_PAT and token handling in catalog-pr

**Severity: low-medium (security/maintenance)**

- A classic PAT (`CATALOG_PAT`) is embedded in a clone URL
  (`https://x-access-token:${GH_TOKEN}@github.com/...`) — if any `set -x`
  sneaks in, the token lands in logs. `gh repo clone`/`gh` commands with
  `GH_TOKEN` env avoid URL-embedded tokens entirely.
- A PAT expires; a GitHub App token (via `actions/create-github-app-token`)
  is the low-maintenance replacement, and the same token could serve
  PIPE-1/PIPE-2.

### PIPE-8 — No scheduled runs against a moving world

**Severity: low**

Nothing runs on a schedule. A weekly `schedule:` run of the e2e suite
against the pinned Kind/node image plus a canary run with the *latest*
node image/console versions catches rot (Cincinnati API changes, console
plugin API drift, base image breakage) before users do. The console
version matrix (currently 4.18–4.22) also needs a periodic reminder to add
new versions and drop old ones — a canary job against the newest release
would surface that automatically.

---

## Suggested order

1. **PIPE-1 + PIPE-2** (distribution — biggest product impact)
2. **PIPE-3** (dependabot — closes known vulns, trivial to add)
3. **PIPE-4 + PIPE-5** (CI efficiency — cost and latency)
4. **PIPE-6, PIPE-7** (release hardening)
5. **PIPE-8** (canary/scheduled runs)

Findings are tracked as issues with the `pipeline` label.
