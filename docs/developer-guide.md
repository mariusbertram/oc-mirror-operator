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
| Go | ≥ 1.25 (`go.mod`) | |
| podman or docker | recent | `CONTAINER_TOOL=podman` is the Makefile default; pass `CONTAINER_TOOL=docker` to override |
| kubectl / oc | ≥ 1.29 | |
| Kind | ≥ 0.25 | local clusters and e2e |
| operator-sdk, opm | ≥ 1.37 | only for bundles/catalogs; `make operator-sdk opm` downloads pinned versions |
| Node.js 20 + npm | | console plugin only |
| controller-gen, kustomize, golangci-lint, setup-envtest | pinned | downloaded into `bin/` by the Makefile on demand |

## Build

```bash
make build                   # generate + manifests + fmt + vet + all Go binaries into bin/
make test                    # unit tests incl. envtest-based controller tests
make lint                    # golangci-lint (same version as CI)
make generate manifests      # after changing api/v1alpha1 or RBAC markers
```

### Images

Five images: controller, manager, worker, plugin, and the bundle.

```bash
export IMAGE_TAG_BASE=quay.io/<you>/oc-mirror-operator VERSION=dev

make docker-build-all docker-push-all       # controller, manager, worker, plugin
make docker-build-controller                # or one at a time: -manager, -worker, -plugin
make docker-buildx                          # multi-arch (linux/amd64, linux/arm64) via buildx
```

This produces `${IMAGE_TAG_BASE}-controller:v${VERSION}` etc. (`IMG_CONTROLLER`,
`IMG_MANAGER`, `IMG_WORKER`, `IMG_PLUGIN` override individual references).

### OLM bundle and catalog

```bash
make bundle IMAGE_TAG_BASE=... VERSION=...      # regenerates bundle/ from config/manifests
make bundle-build bundle-push
make catalog-build catalog-push                 # FBC catalog image containing the bundle
```

Never edit `bundle/` by hand; change `config/manifests/bases/oc-mirror.clusterserviceversion.yaml`
and regenerate.

## Run against a cluster

### Controller on your machine, everything else in the cluster

```bash
make install                                  # CRDs
MANAGER_IMAGE=... WORKER_IMAGE=... PLUGIN_IMAGE=... OPERATOR_IMAGE=... \
OPERATOR_NAMESPACE=oc-mirror-operator make run
```

The controller reads the images it should use for managers, workers, catalog/export
Jobs and the plugin from these variables; the pods it creates run in the cluster, so
the images must be pullable there.

### Kind

```bash
kind create cluster --name oc-mirror-e2e
make docker-build-all IMAGE_TAG_BASE=localhost/oc-mirror-operator VERSION=dev
for c in controller manager worker plugin; do
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
component. The controller reads `MANAGER_IMAGE`, `WORKER_IMAGE`, `PLUGIN_IMAGE` and
`OPERATOR_IMAGE` from its environment, and OLM merges `Subscription.spec.config.env`
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
| Plugin | `oc delete deployment oc-mirror-plugin -n oc-mirror-operator` — recreated immediately. |

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
npm --prefix ui install
make dev-certs                     # self-signed cert for the backend (dev-certs/, git-ignored)
make run-plugin                    # backend on :9443 using your kubeconfig (OPERATOR_NAMESPACE=... to change ns)
npm --prefix ui run dev            # frontend on :9002, proxies /api to :9443 (API_URL=... to change)
```

No cluster at hand: `make run-plugin-mock` serves the UI with in-process mock data,
including the write endpoints (they answer `204`).

| Change | Needed |
|---|---|
| `ui/src/**` | nothing, hot reload |
| `pkg/resourceapi`, `cmd/dashboard` | restart `make run-plugin` |
| `api/v1alpha1` | `make generate manifests`, restart backend |

Quality gates for the UI: `npm --prefix ui run lint`, `npx --prefix ui tsc --noEmit -p ui/tsconfig.json`,
`npm --prefix ui run build:plugin`. CI additionally runs the plugin against a real
`origin-console` bridge in Kind (`hack/plugin-smoke/`) — the only check that catches
runtime mismatches between what the plugin bundles and what the console federates.

To load the plugin into a real console shell, run the OpenShift console `bridge` binary
with `--plugin=oc-mirror-operator=https://localhost:9443` (see the console repository
for the off-cluster flags).

## Useful environment variables

| Variable | Read by | Meaning |
|---|---|---|
| `MANAGER_IMAGE`, `WORKER_IMAGE`, `PLUGIN_IMAGE`, `OPERATOR_IMAGE` | controller | Images for manager/resource-api, worker/cleanup, plugin, catalog/export builders. The controller refuses to start without the first three. |
| `OPERATOR_NAMESPACE` / `POD_NAMESPACE` | controller, plugin backend | Namespace to watch. |
| `WORKER_IMAGE`, `DOCKER_CONFIG` | manager | Worker image; credential directory (set from `authSecret`). |
| `MIRROR_BATCH`, `MANAGER_URL`, `WORKER_TOKEN`, `POD_NAME` | worker | Injected by the manager. |
| `SOURCE_CATALOG`, `TARGET_REF`, `CATALOG_INCLUDE_CONFIG`, `INSECURE_HOSTS` | catalog-builder | Injected by the controller. |
| `MIRROR_SPEC`, `DEST_REGISTRY`, `EXPORT_NAME`, `ARTIFACTS_CONFIGMAP` | export-builder | Injected by the controller. |
