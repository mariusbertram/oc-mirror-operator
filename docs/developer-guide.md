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

## 6. Troubleshooting

- **Image Pull Issues**: Ensure all 3 modular images are accessible from the cluster.
- **RBAC Errors**: Check controller logs if it fails to create Manager deployments.
- **Resource API 404s**: Ensure the `MirrorTarget` has `expose` configured correctly.
