# Developer guide

How to build the operator, run it against a cluster, iterate on one component, and work
on the console plugin. For the contribution process (tests, lint, CI, releases) see
[Contributing](contributing.md).

**Contents**

- [Prerequisites](#prerequisites)
- [Build](#build)
- [Run against a cluster](#run-against-a-cluster)
- [Iterate on a single component under OLM](#iterate-on-a-single-component-under-olm)
- [Console plugin development](#console-plugin-development)
- [Useful environment variables](#useful-environment-variables)

---

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | >= 1.27.1 (`go.mod`) | container builds and the devcontainer use the same pinned toolchain |
| podman or docker | recent | `CONTAINER_TOOL=podman` is the Makefile default; pass `CONTAINER_TOOL=docker` to override |
| kubectl / oc | ≥ 1.29 | |
| Kind | ≥ 0.25 | local clusters and e2e |
| operator-sdk | 1.42.3 | `make operator-sdk` downloads and checksum-verifies the pinned version, independent of a system installation |
| opm | 1.74.0 | only for catalogs; `make opm` downloads and checksum-verifies the pinned version |
| Node.js 22.12+ + npm | | console plugin only; container builds use Node.js 26 |
| controller-gen, kustomize, golangci-lint, setup-envtest | pinned | downloaded into `bin/` by the Makefile on demand |

## Build

```bash
make build                   # generate + manifests + fmt + vet + all Go binaries into bin/
make test                    # unit tests incl. envtest-based controller tests
make lint                    # golangci-lint (same version as CI)
make generate manifests      # after changing api/v1alpha1 or RBAC markers
```

### Images

Nine images: controller, manager, worker, five versioned plugins, and the bundle.

```bash
export IMAGE_TAG_BASE=quay.io/<you>/oc-mirror-operator VERSION=dev

make build-images push-images              # controller, manager, worker, all five plugins
make docker-build-plugin CONSOLE_VERSION=4.18  # one matching plugin
make docker-buildx                          # multi-arch (linux/amd64, linux/arm64) via buildx
```

This produces `${IMAGE_TAG_BASE}-controller:v${VERSION}` etc. (`IMG_CONTROLLER`,
`IMG_MANAGER`, `IMG_WORKER` override individual references). Plugin tags are
`${IMAGE_TAG_BASE}-plugin:v${VERSION}-ocp4.18` through `-ocp4.22`.
`IMG_PLUGIN_4_18` through `IMG_PLUGIN_4_22` override the install/bundle references;
`IMG_PLUGIN` overrides only the single `docker-build-plugin`/`docker-push-plugin` target.

### OLM bundle and catalog

```bash
make bundle IMAGE_TAG_BASE=... VERSION=...      # regenerates bundle/ from config/manifests
make bundle-build bundle-push
make catalog-build catalog-push                 # FBC catalog image containing the bundle
```

Never edit `bundle/` by hand; change `config/manifests/bases/oc-mirror.clusterserviceversion.yaml`
and regenerate.

Before submitting to OperatorHub.io or the OpenShift community catalog, publish
the versioned component images and regenerate the release bundle with
`USE_IMAGE_DIGESTS=true` (the default). This resolves the runtime images to digests
and populates `spec.relatedImages` for disconnected mirroring. A local bundle
generated with `USE_IMAGE_DIGESTS=false` is only a development artifact, not the
catalog submission.

```bash
make bundle VERSION=<release-version> IMAGE_TAG_BASE=<public-registry>/<image-base>
bin/operator-sdk bundle validate ./bundle --select-optional suite=operatorframework
```

## Run against a cluster

### Controller on your machine, everything else in the cluster

```bash
make install                                  # CRDs
MANAGER_IMAGE=... WORKER_IMAGE=... OPERATOR_IMAGE=... \
RELATED_IMAGE_PLUGIN_4_18=... RELATED_IMAGE_PLUGIN_4_19=... \
RELATED_IMAGE_PLUGIN_4_20=... RELATED_IMAGE_PLUGIN_4_21=... RELATED_IMAGE_PLUGIN_4_22=... \
OPERATOR_NAMESPACE=oc-mirror-operator make run
```

The controller reads the images it should use for managers, workers, catalog/export
Jobs and the plugin from these variables; the pods it creates run in the cluster, so
the images must be pullable there.

### Kind

```bash
kind create cluster --name oc-mirror-e2e
make docker-build-all IMAGE_TAG_BASE=localhost/oc-mirror-operator VERSION=dev
for c in controller manager worker; do
  kind load docker-image localhost/oc-mirror-operator-$c:vdev --name oc-mirror-e2e    # podman: kind load image-archive
done
make install deploy IMG=localhost/oc-mirror-operator-controller:vdev

kubectl apply -f config/samples/registry_deploy.yaml      # throwaway in-cluster registry (insecure)
kubectl apply -f config/samples/imageset_test_small.yaml \
              -f config/samples/mirror_target_sample.yaml -n oc-mirror-operator
```

`config/samples/mirror_target_sample.yaml` points at `registry.default.svc.cluster.local:5000`
with `insecure: true`, which matches the sample registry. There is no `Route` API on
Kind; the Resource API is reachable with `kubectl port-forward svc/oc-mirror-resource-api 8081`.

`make test-e2e-cluster` does the build/load/deploy/test cycle in one go; see
[Contributing → E2E tests](contributing.md#e2e-tests).

### OpenShift via OLM

```bash
make bundle bundle-build bundle-push IMAGE_TAG_BASE=quay.io/<you>/oc-mirror-operator VERSION=dev
bin/operator-sdk run bundle quay.io/<you>/oc-mirror-operator-bundle:vdev -n oc-mirror-operator
```

or `make deploy-test`, which builds and pushes all images, the bundle and a catalog and
deploys through a CatalogSource.

## Iterate on a single component under OLM

With the operator installed by OLM you do not need a new bundle to test one changed
component. The controller reads `MANAGER_IMAGE`, `WORKER_IMAGE`, `OPERATOR_IMAGE`,
and `RELATED_IMAGE_PLUGIN_4_18` through `RELATED_IMAGE_PLUGIN_4_22`
from its environment, and OLM merges `Subscription.spec.config.env`
into the controller Deployment:

```bash
make docker-build-manager docker-push-manager IMG_MANAGER=quay.io/<you>/oc-mirror-operator-manager:dev

oc patch subscription oc-mirror -n oc-mirror-operator --type merge -p '
{"spec":{"config":{"env":[{"name":"MANAGER_IMAGE","value":"quay.io/<you>/oc-mirror-operator-manager:dev"}]}}}'

oc rollout status deployment/oc-mirror-operator-controller-manager -n oc-mirror-operator
```

Then make the controller recreate the children that use the image:

| Component | How the new image gets picked up |
|---|---|
| Manager | The controller updates the manager Deployment on its next reconcile (≤ 10 min) — or touch the MirrorTarget (`kubectl annotate mirrortarget <name> dev=$(date +%s) --overwrite`). |
| Worker | New worker pods use the new image automatically. |
| Catalog builder (`OPERATOR_IMAGE`) | The image is part of the build signature; catalogs rebuild on the next reconcile once nothing is pending. |
| Plugin | Override the `RELATED_IMAGE_PLUGIN_4_*` variable matching the Console release, then `oc delete deployment oc-mirror-plugin -n oc-mirror-operator` to reconcile immediately. Do not substitute a different release's plugin. |

Remove the override with `-p '{"spec":{"config":{"env":[]}}}'` to return to the bundle images.

## Console plugin development

The plugin is a React/PatternFly app in `ui/`, served by the Go backend in
`cmd/dashboard` (which also proxies the REST API with the caller's token).

```
Browser ─▶ http://localhost:9002   webpack dev server (hot reload)
              └─ /api/* ─────────▶ https://localhost:9443   go run ./cmd/dashboard
                                          └─ kubeconfig ──▶ cluster
```

```bash
npm --prefix ui ci
make dev-certs                     # self-signed cert for the backend (dev-certs/, git-ignored)
make run-plugin                    # backend on :9443 using your kubeconfig (OPERATOR_NAMESPACE=... to change ns)
npm --prefix ui run dev            # frontend on :9002, proxies /api to :9443 (API_URL=... to change)
```

No cluster at hand: run `npm --prefix ui run dev:mock` (or `make run-plugin-mock`)
to serve the UI at `http://localhost:9002` with in-process mock data. No backend,
OpenShift cluster, or kubeconfig is needed; mock write endpoints respond locally
so the edit and action flows can be exercised without changing cluster resources.
Mock writes acknowledge actions without persisting changes to the sample data. After installing
a production profile, run `npm --prefix ui ci` to restore the default standalone
React 17 / Router 5 harness before starting mock mode.

### Versioned production builds

The supported Console minors are **4.18, 4.19, 4.20, 4.21, and 4.22**.
`ui/console-compatibility.json` pins each release's SDK, React, Router, and
PatternFly dependencies. The operator reads the `operator` version in the
`console` ClusterOperator's `status.versions`, not the cluster-wide upgrade
version. It updates the plugin Deployment when that Console version changes.
An unsupported release, missing/malformed Console version, or missing matching
image unregisters the plugin and removes its workloads/RBAC; transient API
errors retain the current deployment and retry. There is no cross-version
fallback. Non-OpenShift clusters skip plugin deployment.

Build plugin assets on Linux (the SDK's dynamic-module paths are not portable
to Windows); Windows supports the standalone/mock workflow:

```bash
export CONSOLE_VERSION=4.18
npm --prefix ui run install:console-profile
npx --prefix ui tsc --noEmit -p ui/tsconfig.console.json
npm --prefix ui run build:plugin
# Or use a Linux container through Podman/Docker:
make docker-build-plugin CONSOLE_VERSION=4.18
```

The installer resolves one complete profile without modifying the checked-in
manifest or default lockfile. It records and checks installed versions, generates
a git-ignored TypeScript config checking the real Router/PatternFly adapters,
and rejects profile/build mismatches. Do not run installs concurrently or share
`node_modules` between Windows and Linux containers.

| Change | Needed |
|---|---|
| `ui/src/**` | nothing, hot reload |
| `pkg/resourceapi`, `cmd/dashboard` | restart `make run-plugin` |
| `api/v1alpha1` | `make generate manifests`, restart backend |

Quality gates for the UI: `npm --prefix ui run lint`, `npx --prefix ui tsc --noEmit -p ui/tsconfig.json`,
and, after profile installation, the profile-specific typecheck/build above.
CI additionally runs each profile against a matching real
`origin-console` bridge in Kind (`hack/plugin-smoke/`) — the only check that catches
runtime mismatches between what the plugin bundles and what the console federates.

### Theme regression coverage

Each of the five Console smoke jobs exercises **Light and Dark** through the
Console's **User Preferences → General → Theme** control. The browser emulates
the opposite OS preference and reloads before checking the persisted Console
preference, real PF5/PF6 dark classes, and resolved theme tokens. Changing only
`prefers-color-scheme` or injecting a dark class does not establish coverage.

Plugin-local compatibility variables map legacy references to PF6 semantic
tokens or PF5 fallbacks inside `ThemePageSection`; they do not redefine the
Console's root theme. Native selects follow the selected Console theme even
when it differs from the OS. The smoke measures rendered text contrast,
selected-row indicators, pane backgrounds/borders, select/option colors,
dialogs, and filter/Refresh controls, including hover and narrow layouts.
Screenshots and measurements are saved under each artifact's `light/` and
`dark/` directories.

Run this harness only against a dedicated test cluster: it recreates its named
fixtures in `oc-mirror-demo` between themes and confirms destructive actions.
Native popup styling is browser/OS-dependent; headless Chromium verifies
computed option colors and `color-scheme`, not the pixels of every OS-rendered
popup. Decorative border colors are checked against the theme tokens, not
treated as independent 3:1 control indicators. These checks are regression
coverage, not a complete accessibility audit.

### Kind, minc, and full OpenShift coverage

The standalone mock harness checks page layout and simulated API flows, but it
does **not** load the Console's federated dependencies. It cannot establish
Console compatibility. Browser smoke tests against the matching real
`origin-console` bridge remain required for every supported profile.

[minc](https://github.com/minc-org/minc) runs MicroShift in a container and is a
reasonable optional integration environment. MicroShift's ingress router and
service CA can add coverage for OpenShift-specific routing and serving
certificates that vanilla Kind does not provide. Installing a standalone Console
on minc would still exercise the same real Console frontend as the existing
Kind/bridge smoke matrix; changing the cluster alone does not improve federation
coverage.

MicroShift is a minimal OpenShift distribution, not a full OpenShift cluster.
Do not assume that adding a Console Deployment also provides console-operator
reconciliation, the `console` ClusterOperator/version lifecycle, automatic
ConsolePlugin registration, or the full OpenShift authentication integrations.
Manually supplying these resources only exercises the supplied fixture, not
those absent integrations. Use full OpenShift for end-to-end coverage of those
operator-managed flows.

Keep the current Kind smoke matrix. Consider an additional minc integration
suite only for a specific routing/certificate gap, and measure reproducibility,
version availability, startup cost, and coverage before replacing existing CI.
This is a design consideration, not a validated minc replacement.

To load the plugin into a real console shell, run the OpenShift console `bridge` binary
with `--plugin=oc-mirror-operator=https://localhost:9443` (see the console repository
for the off-cluster flags).

## Useful environment variables

| Variable | Read by | Meaning |
|---|---|---|
| `MANAGER_IMAGE`, `WORKER_IMAGE`, `OPERATOR_IMAGE` | controller | Images for manager/resource-api, worker/cleanup, catalog/export builders. |
| `RELATED_IMAGE_PLUGIN_4_18` … `RELATED_IMAGE_PLUGIN_4_22` | controller | Matching Console plugin images (OLM may supply digest references). Missing matching image disables/unregisters the plugin, not the controller. |
| `OPERATOR_NAMESPACE` / `POD_NAMESPACE` | controller, plugin backend | Namespace to watch. |
| `WORKER_IMAGE`, `DOCKER_CONFIG` | manager | Worker image; credential directory (set from `authSecret`). |
| `MIRROR_BATCH`, `MANAGER_URL`, `WORKER_TOKEN`, `POD_NAME` | worker | Injected by the manager. |
| `SOURCE_CATALOG`, `TARGET_REF`, `CATALOG_INCLUDE_CONFIG`, `INSECURE_HOSTS` | catalog-builder | Injected by the controller. |
| `MIRROR_SPEC`, `DEST_REGISTRY`, `EXPORT_NAME`, `ARTIFACTS_CONFIGMAP` | export-builder | Injected by the controller. |
