# Quick Start: Bootstrap the Bootstrapper (Disconnected Install of the Operator Itself)

**Use case:** You are standing up a *fully disconnected* cluster and want to run
oc-mirror-operator there. The operator's own images must exist in your target
registry *before* it can run — a chicken-and-egg problem this recipe solves.

---

## Overview

The operator runs four images it references at runtime:

| Component | Default image (release `v<version>`) | Env var the controller reads |
|---|---|---|
| Controller | `ghcr.io/mariusbertram/oc-mirror-operator-controller:v<version>` | — (its own image) |
| Manager / resource API | `ghcr.io/mariusbertram/oc-mirror-operator-manager:v<version>` | `MANAGER_IMAGE` |
| Worker / cleanup jobs | `ghcr.io/mariusbertram/oc-mirror-operator-worker:v<version>` | `WORKER_IMAGE` |
| Console plugin (OpenShift only) | `ghcr.io/mariusbertram/oc-mirror-operator-plugin:v<version>-ocp<console>` | `RELATED_IMAGE_PLUGIN_4_18` … `RELATED_IMAGE_PLUGIN_4_22` (one per console version) |

In a disconnected environment none of these registries are reachable. The
recipe below mirrors the operator images into your local registry with
`skopeo` (works from any machine with access to both registries) and then
points the operator at the local copies through env overrides.

## Step 1: Mirror the operator images (connected side)

Run on a machine that can reach both ghcr.io and your target registry.
Digest-pinned references are taken from the GitHub release notes of the
version you are installing.

```bash
TARGET=registry.example.local/oc-mirror-operator
VERSION=v0.1.0

for img in controller manager worker; do
  skopeo copy --all \
    "docker://ghcr.io/mariusbertram/oc-mirror-operator-${img}:${VERSION}" \
    "docker://${TARGET}-${img}:${VERSION}"
done

# Console plugin images (only needed on OpenShift; one per console version)
for console in 4.18 4.19 4.20 4.21 4.22; do
  skopeo copy --all \
    "docker://ghcr.io/mariusbertram/oc-mirror-operator-plugin:${VERSION}-ocp${console}" \
    "docker://${TARGET}-plugin:${VERSION}-ocp${console}"
done
```

Alternatively, if you already have oc-mirror-operator running on a connected
cluster, create an `ImageSet` for the four operator images and mirror it the
regular way — the operator can mirror itself.

## Step 2: Install the operator with local image references

Use the static release manifest and rewrite the image references, or install
the rendered manifest with overridden env vars:

```bash
# Fetch the release manifest and rewrite the images to your registry
curl -fsSL -o oc-mirror-operator.yaml \
  https://github.com/mariusbertram/oc-mirror-operator/releases/download/${VERSION}/oc-mirror-operator.yaml

sed -i "s|ghcr.io/mariusbertram/oc-mirror-operator|${TARGET}|g" oc-mirror-operator.yaml

kubectl apply -f oc-mirror-operator.yaml
```

The controller reads `MANAGER_IMAGE`, `WORKER_IMAGE`, `OPERATOR_IMAGE` and
the five `RELATED_IMAGE_PLUGIN_4_18` … `RELATED_IMAGE_PLUGIN_4_22` variables
from its own container env (set by the rendered manifest). After the rewrite,
they all point at your local registry — verify:

```bash
kubectl set env deployment/oc-mirror-controller-manager -n oc-mirror-system --list | grep IMAGE
```

## Step 3: Verify

```bash
kubectl rollout status deployment/oc-mirror-controller-manager -n oc-mirror-system
kubectl get pods -n oc-mirror-system
```

If you see `ImagePullBackOff` for the manager/worker/plugin pods, the env
overrides still point at ghcr.io — re-check Step 2 (see
[troubleshooting](troubleshooting.md)).

## After the bootstrap

From here on, the operator itself can maintain your mirrors: create
`ImageSet` and `MirrorTarget` resources as described in the
[getting-started guide](getting-started.md). Future operator upgrades repeat
this recipe for the new version's images.

---

**Related guides**

| Task | Guide |
|---|---|
| First steps after install | [Getting Started](getting-started.md) |
| Full disconnected setup | [Disconnected](quickstart-disconnected.md) |
| Troubleshooting | [Troubleshooting](troubleshooting.md) |
