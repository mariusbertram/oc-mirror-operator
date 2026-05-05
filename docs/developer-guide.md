# Developer Guide

This guide describes how to build, deploy, and test the `oc-mirror-operator` in various environments.

---

## 1. Prerequisites

Ensure you have the following tools installed:

- **Go**: ≥ 1.24
- **Container Tool**: `podman` (preferred) or `docker`
- **Operator SDK**: ≥ 1.37
- **kubectl** & **oc** (for OpenShift)
- **KinD**: For local Kubernetes testing
- **Kustomize**: (Auto-installed via Makefile)
- **controller-gen**: (Auto-installed via Makefile)

---

## 2. Building the Operator

### 2.1 Modular Architecture
The operator uses three separate components, each with its own binary and image:
- **Controller**: The main operator reconciliation loop.
- **Manager**: Per-MirrorTarget orchestration pod.
- **Worker**: Ephemeral pods that perform the actual mirroring.

### 2.2 Local Binaries
```bash
# Build all binaries
make build

# Build individual components
make build/controller
make build/manager
make build/worker
```

### 2.3 Container Images
Build and push all three images to your registry:

```bash
export REGISTRY=quay.io/your-user
export VERSION=v0.0.1

# Build and push all images
make docker-build-all docker-push-all \
  IMAGE_TAG_BASE=${REGISTRY}/oc-mirror-operator \
  VERSION=${VERSION}
```

This will create:
- `${REGISTRY}/oc-mirror-operator-controller:${VERSION}`
- `${REGISTRY}/oc-mirror-operator-manager:${VERSION}`
- `${REGISTRY}/oc-mirror-operator-worker:${VERSION}`

---

## 3. Generating the OLM Bundle

After building and pushing the modular images, you must generate the OLM bundle manifests:

```bash
make bundle \
  IMAGE_TAG_BASE=${REGISTRY}/oc-mirror-operator \
  VERSION=${VERSION} \
  IMG_CONTROLLER=${REGISTRY}/oc-mirror-operator-controller:${VERSION} \
  IMG_MANAGER=${REGISTRY}/oc-mirror-operator-manager:${VERSION} \
  IMG_WORKER=${REGISTRY}/oc-mirror-operator-worker:${VERSION}
```

### Build and Push the Bundle Image:
```bash
make bundle-build bundle-push \
  BUNDLE_IMG=${REGISTRY}/oc-mirror-operator-bundle:${VERSION}
```

---

## 4. Deployment Strategies

### 4.1 OpenShift (via OLM)
Recommended for development and production on OpenShift.

```bash
# Create a namespace
oc create namespace oc-mirror-operator-test

# Run the bundle using operator-sdk
./bin/operator-sdk run bundle ${REGISTRY}/oc-mirror-operator-bundle:${VERSION} \
  --namespace oc-mirror-operator-test \
  --timeout 10m
```

### 4.2 MicroShift
MicroShift is resource-constrained. You can deploy via OLM (if enabled) or direct manifests.

**Direct Manifest Deployment:**
```bash
# Update images in config/manager/kustomization.yaml or use environment variables
make deploy IMG=${REGISTRY}/oc-mirror-operator-controller:${VERSION}
```

**Notes for MicroShift:**
- Ensure `workerStorage` uses `emptyDir` (default) to avoid external storage requirements.
- Use a local registry or ensure the MicroShift node has access to your external registry.

### 4.3 KinD (Local Kubernetes)
Ideal for rapid development cycles.

```bash
# 1. Create cluster with local registry
./hack/kind-with-registry.sh

# 2. Build and load images
make docker-build-all IMAGE_TAG_BASE=localhost:5001/oc-mirror-operator VERSION=dev
kind load docker-image localhost:5001/oc-mirror-operator-controller:dev
kind load docker-image localhost:5001/oc-mirror-operator-manager:dev
kind load docker-image localhost:5001/oc-mirror-operator-worker:dev

# 3. Deploy
make deploy IMG=localhost:5001/oc-mirror-operator-controller:dev
```

### 4.4 "Normal" Kubernetes
Deployment on vanilla Kubernetes works similarly to KinD but usually requires an external Ingress controller.

```bash
# Deploy CRDs and Operator
make install deploy IMG=${REGISTRY}/oc-mirror-operator-controller:${VERSION}

# Configure Ingress for the Resource API (Web UI)
# Set expose.type=Ingress in your MirrorTarget
```

---

## 5. Testing

### 5.1 Unit Tests
```bash
make test
```

### 5.2 End-to-End (E2E) Tests
E2E tests require a running cluster (KinD recommended).

```bash
# Set the image to test
export IMG=${REGISTRY}/oc-mirror-operator-controller:${VERSION}
make test-e2e
```

### 5.3 Manual Testing
Create a `MirrorTarget` and `ImageSet` using the samples:
```bash
kubectl apply -f config/samples/mirror_v1alpha1_imageset.yaml
kubectl apply -f config/samples/mirror_v1alpha1_mirrortarget.yaml
```

Check the status via the Web UI (Port-forwarding):
```bash
kubectl port-forward service/oc-mirror-resource-api 8081:8081
# Open http://localhost:8081/ui/
```

---

## 6. Iterative Development: Testing a Single Component via OLM

When an OLM-managed operator is already running, you do **not** need to rebuild the bundle or catalog to test a change in a single component. OLM forwards any `env` entries defined in the `Subscription` directly into the controller-manager Deployment, which causes a rolling restart. Because the operator reads `DASHBOARD_IMAGE`, `MANAGER_IMAGE`, `WORKER_IMAGE`, `OPERATOR_IMAGE`, and `OAUTH_PROXY_IMAGE` from its own environment at runtime, you only need to:

1. Build and push the modified image.
2. Patch the Subscription with the new image reference.
3. Verify that the downstream workloads use the new image.

### 6.1 Build and Push a Single Component

Each component has its own Makefile target. Override `IMG_<COMPONENT>` to point to your personal dev tag:

```bash
# Dashboard only
make docker-build-dashboard docker-push-dashboard \
  IMG_DASHBOARD=quay.io/your-user/oc-mirror-operator-dashboard:dev

# Manager only
make docker-build-manager docker-push-manager \
  IMG_MANAGER=quay.io/your-user/oc-mirror-operator-manager:dev

# Worker / cleanup (same image, different subcommand)
make docker-build-worker docker-push-worker \
  IMG_WORKER=quay.io/your-user/oc-mirror-operator-worker:dev

# Controller (operator reconciliation loop)
make docker-build-controller docker-push-controller \
  IMG_CONTROLLER=quay.io/your-user/oc-mirror-operator-controller:dev
```

### 6.2 Override the Image via the Subscription

Patch the `Subscription` with `spec.config.env` to replace the desired env var. OLM will detect the change and roll out a new controller-manager pod automatically.

```bash
# Replace only the dashboard image (all other images stay at their bundle-defined values)
oc patch subscription oc-mirror \
  -n oc-mirror-operator \
  --type merge \
  -p '{
    "spec": {
      "config": {
        "env": [
          {
            "name": "DASHBOARD_IMAGE",
            "value": "quay.io/your-user/oc-mirror-operator-dashboard:dev"
          }
        ]
      }
    }
  }'
```

To override multiple components at once, extend the `env` list:

```bash
oc patch subscription oc-mirror \
  -n oc-mirror-operator \
  --type merge \
  -p '{
    "spec": {
      "config": {
        "env": [
          {"name": "MANAGER_IMAGE",   "value": "quay.io/your-user/oc-mirror-operator-manager:dev"},
          {"name": "WORKER_IMAGE",    "value": "quay.io/your-user/oc-mirror-operator-worker:dev"}
        ]
      }
    }
  }'
```

> **Note**: OLM merges the Subscription `env` on top of the env vars that are baked into the CSV. You only need to specify the entries you want to change — everything else keeps its original bundle value.

### 6.3 Verify the Rollout

Wait for the controller-manager pod to restart and confirm the env var is set:

```bash
# Watch the rollout
oc rollout status deployment/oc-mirror-controller-manager -n oc-mirror-operator

# Confirm the env var value in the running pod
oc set env deployment/oc-mirror-controller-manager \
  -n oc-mirror-operator --list \
  | grep -E 'DASHBOARD|MANAGER|WORKER|OPERATOR|OAUTH'
```

### 6.4 Trigger Downstream Re-creation

The controller reads the image env vars once on startup and passes them through to child workloads (manager Deployments, worker Pods, dashboard Deployment). After the controller-manager pod restarts, existing child resources are **not** automatically updated — you need to trigger a reconcile:

**Dashboard** (UIConfiguration controller):
```bash
# Delete and recreate the UIConfiguration to force the dashboard Deployment to be rebuilt
oc delete uiconfiguration oc-mirror-dashboard-config -n oc-mirror-operator
oc apply -f config/samples/mirror_v1alpha1_uiconfiguration.yaml
```

**Manager / Worker** (MirrorTarget controller):
```bash
# Annotate the MirrorTarget to force a full reconcile
oc annotate mirrortarget <name> \
  -n <namespace> \
  mirror.openshift.io/recollect=true \
  --overwrite
```

### 6.5 Revert to Bundle-Defined Images

Remove the `spec.config.env` override to let OLM restore the original bundle images:

```bash
oc patch subscription oc-mirror \
  -n oc-mirror-operator \
  --type merge \
  -p '{"spec": {"config": {"env": []}}}'
```

---

## 7. Troubleshooting

- **Image Pull Issues**: Ensure all 3 modular images are accessible from the cluster.
- **RBAC Errors**: Check controller logs if it fails to create Manager deployments.
- **Resource API 404s**: Ensure the `MirrorTarget` has `expose` configured correctly.
