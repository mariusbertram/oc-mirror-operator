# oc-mirror-operator — User Guide

This document describes the complete configuration and operation of the `oc-mirror-operator`.

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [Installation](#2-installation)
   - [Via OLM (recommended)](#21-via-olm-recommended)
   - [Manually via kubectl](#22-manually-via-kubectl)
3. [Concepts](#3-concepts)
   - [MirrorTarget](#31-mirrortarget)
   - [ImageSet](#32-imageset)
   - [Manager Pod](#33-manager-pod)
   - [Worker Pods](#34-worker-pods)
   - [Image State (ConfigMap)](#35-image-state-configmap)
4. [Quick Start](#4-quick-start)
5. [Configuring Registry Credentials](#5-configuring-registry-credentials)
   - [Standard Kubernetes Pull Secret](#51-standard-kubernetes-pull-secret)
   - [Username/Password Secret](#52-usernamepassword-secret)
   - [Combining Multiple Registries](#53-combining-multiple-registries)
6. [ImageSet Configuration](#6-imageset-configuration)
   - [OpenShift Releases](#61-openshift-releases)
   - [Operator Catalogs](#62-operator-catalogs)
   - [Additional Images](#63-additional-images)
7. [MirrorTarget Configuration](#7-mirrortarget-configuration)
   - [Basic Configuration](#71-basic-configuration)
   - [Performance Tuning](#72-performance-tuning)
   - [Polling and CheckExist Intervals](#73-polling-and-checkexist-intervals)
   - [Expose (Resource Server)](#74-expose-resource-server)
   - [Pod Resources and Placement](#75-pod-resources-and-placement)
   - [Image Cleanup on Removal](#76-image-cleanup-on-removal)
   - [HTTP Proxy Configuration](#77-http-proxy-configuration)
   - [Custom CA Bundle](#78-custom-ca-bundle)
   - [Worker Storage for Large Images](#79-worker-storage-for-large-images)
8. [Monitoring Status](#8-monitoring-status)
   - [MirrorTarget Status](#81-mirrortarget-status)
   - [ImageSet Status](#82-imageset-status)
   - [Failed Images](#83-failed-images)
9. [Operations and Maintenance](#9-operations-and-maintenance)
   - [Recollect (Force Re-sync)](#91-recollect-force-re-sync)
   - [Cleanup (Delete Images)](#92-cleanup-delete-images)
   - [ImageSet Changes](#93-imageset-changes)
   - [Image Retries and Permanent Failures](#94-image-retries-and-permanent-failures)
10. [Resource Server](#10-resource-server)
    - [Retrieving IDMS and ITMS](#101-retrieving-idms-and-itms)
    - [CatalogSource and ClusterCatalog](#102-catalogsource-and-clustercatalog)
    - [Release Signatures](#103-release-signatures)
11. [Full Configuration Reference](#11-full-configuration-reference)
    - [ImageSet Fields](#111-imageset-fields)
    - [MirrorTarget Fields](#112-mirrortarget-fields)
12. [Examples](#12-examples)
13. [Troubleshooting](#13-troubleshooting)
    - [HTTP Proxy Issues](#135-http-proxy-issues)

---

## 1. Prerequisites

| Requirement | Minimum Version | Notes |
|-------------|----------------|-------|
| Kubernetes | 1.26+ | Or OpenShift 4.12+ |
| OLM | 0.22+ | Required only for OLM-based installation |
| Target Registry | — | Write access required; Quay, Harbor, distribution/registry tested |
| Network Access | — | Manager and worker pods require access to both source and target registry |

The operator does **not** require persistent storage (no PVC). The image state is stored in Kubernetes ConfigMaps.

---

## 2. Installation

### 2.1 Via OLM (recommended)

#### Step 1: Create the CatalogSource

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: brtrm-dev-catalog
  namespace: openshift-marketplace   # oder olm namespace
spec:
  sourceType: grpc
  image: quay.io/mariusbertram/brtrm-dev-catalog:latest
  displayName: brtrm Dev Catalog
  publisher: Marius Bertram
```

```bash
kubectl apply -f catalogsource.yaml
# Wait until the CatalogSource pod is Running
kubectl get pods -n openshift-marketplace -l olm.catalogSource=brtrm-dev-catalog
```

#### Step 2: Install the Operator

In the OpenShift Console under **Operators → OperatorHub**, search for `oc-mirror` and install it. The namespace `oc-mirror-operator` will be suggested automatically.

Or via CLI:

```bash
# OperatorGroup for single-namespace installation
cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: oc-mirror-operator-group
  namespace: oc-mirror-operator
spec:
  targetNamespaces:
    - oc-mirror-operator
EOF

# Subscription
cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: oc-mirror-operator
  namespace: oc-mirror-operator
spec:
  channel: stable
  name: oc-mirror
  source: brtrm-dev-catalog
  sourceNamespace: openshift-marketplace
EOF
```

#### Step 3: Verify the Installation

```bash
kubectl get csv -n oc-mirror-operator
# NAME              PHASE
# oc-mirror.v0.0.2  Succeeded

kubectl get pods -n oc-mirror-operator
# NAME                                           READY   STATUS    RESTARTS
# oc-mirror-operator-controller-manager-xxx      1/1     Running   0
```

### 2.2 Manually via kubectl

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# RBAC
kubectl apply -f config/rbac/

# Deploy the operator
kubectl apply -f config/manager/manager.yaml
```

---

## 3. Concepts

### 3.1 MirrorTarget

A `MirrorTarget` defines **where** to mirror to:

- Target registry URL and credentials
- List of `ImageSet` objects to use
- Performance parameters (concurrency, batchSize)
- Polling and CheckExist intervals
- Exposure of the Resource Server (IDMS/ITMS endpoint)

For each `MirrorTarget`, the operator starts **one manager pod** in the same namespace. This manager is responsible for worker orchestration, image state management, and the Resource Server.

#### Multiple MirrorTargets per Namespace

Multiple `MirrorTarget` CRs can coexist in the same namespace. Each `MirrorTarget` creates its own isolated set of RBAC resources (`{mt.Name}-coordinator`, `{mt.Name}-worker`) — there are no naming conflicts.

Use cases:
- Mirror to different target registries from the same namespace
- Separate ImageSets with different concurrency/performance settings
- Isolate different teams' mirror configurations

### 3.2 ImageSet

An `ImageSet` defines **what** to mirror:

- OpenShift release channels (with version filters)
- Operator catalogs (with package filters)
- Individual additional images

An `ImageSet` is independent of a specific target — it is referenced via `MirrorTarget.spec.imageSets`. The same `ImageSet` can be used in a `MirrorTarget`. However, an `ImageSet` should only be referenced by **one** `MirrorTarget`.

### 3.3 Manager Pod

The manager pod is automatically started by the operator controller as a `Deployment` when a `MirrorTarget` is created. It:

- Resolves the upstream image list (Cincinnati API, catalog index)
- Manages the image state (which images are still pending, which have been mirrored)
- Starts worker pods for the actual mirroring
- Periodically checks whether mirrored images are still present in the target registry
- Provides the Resource Server for IDMS/ITMS/CatalogSource

### 3.4 Worker Pods

Worker pods are ephemeral — they are started for a batch of images and automatically cleaned up after completion. They:

- Receive their work (batch of source→dest pairs) as a JSON annotation
- Authenticate with the manager via bearer token
- Report each success/failure individually back to the manager
- Check before mirroring whether the image is still needed (the spec may have changed in the meantime)

### 3.5 Image State (ConfigMap)

The complete mirroring progress is stored in a Kubernetes ConfigMap (`<imageset-name>-images` in the same namespace). The data is gzip-compressed (JSON), resulting in approximately 30 bytes per image — scales easily to 50,000+ images.

Each entry contains:
- `source`: Source image reference
- `state`: `Pending` | `Mirrored` | `Failed`
- `retryCount`: Number of previous failed attempts
- `permanentlyFailed`: true after 10 failed attempts
- `lastError`: last error message
- `origin`: Source (release/operator/additional)
- `originRef`: Human-readable description of the spec entry that produced this image

---

## 4. Quick Start

This example mirrors OpenShift 4.14 and the `web-terminal` operator into a private Quay registry.

**Step 1: Create the namespace**

```bash
kubectl create namespace mein-mirror
```

**Step 2: Create registry credentials**

```bash
# Credentials for both: source registry (registry.redhat.io) and target registry
kubectl create secret docker-registry mirror-creds \
  --docker-server=registry.redhat.io \
  --docker-username=<RH-Username> \
  --docker-password=<RH-Token> \
  -n mein-mirror

# For Quay as target: separate secret or combined (see section 5)
kubectl create secret docker-registry mirror-creds \
  --docker-server=quay.example.com \
  --docker-username=<Quay-User> \
  --docker-password=<Quay-Password> \
  -n mein-mirror
```

**Step 3: Create the ImageSet**

```yaml
# imageset-ocp414.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-14
  namespace: mein-mirror
spec:
  mirror:
    platform:
      architectures:
        - amd64
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
          maxVersion: "4.14.35"
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
```

```bash
kubectl apply -f imageset-ocp414.yaml
```

**Step 4: Create the MirrorTarget**

```yaml
# mirrortarget-quay.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: quay-mirror
  namespace: mein-mirror
spec:
  registry: quay.example.com/mein-mirror
  authSecret: mirror-creds
  imageSets:
    - ocp-4-14
```

```bash
kubectl apply -f mirrortarget-quay.yaml
```

**Step 5: Observe progress**

```bash
# Overall overview
kubectl get mirrortarget,imageset -n mein-mirror

# Detailed progress
kubectl get imageset ocp-4-14 -n mein-mirror -o json | jq '.status'

# Manager logs
kubectl logs deployment/quay-mirror-manager -n mein-mirror -f

# Watch worker pods
kubectl get pods -n mein-mirror -w
```

---

## 5. Configuring Registry Credentials

The operator supports two secret formats.

### 5.1 Standard Kubernetes Pull Secret

The most common format: `kubernetes.io/dockerconfigjson` secret with the key `.dockerconfigjson`. This format allows credentials for **multiple registries** in a single secret.

```bash
kubectl create secret docker-registry meine-creds \
  --docker-server=quay.example.com \
  --docker-username=<user> \
  --docker-password=<password> \
  -n mein-mirror
```

Or copy directly from the OpenShift cluster pull secret:

```bash
# Export pull secret from openshift-config
oc get secret pull-secret -n openshift-config \
  -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d > /tmp/pull-secret.json

# Create the secret in the target namespace
kubectl create secret generic mirror-pull-secret \
  --from-file=.dockerconfigjson=/tmp/pull-secret.json \
  --type=kubernetes.io/dockerconfigjson \
  -n mein-mirror
```

### 5.2 Username/Password Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: meine-creds
  namespace: mein-mirror
type: Opaque
stringData:
  username: mein-user
  password: mein-passwort
```

### 5.3 Combining Multiple Registries

To bundle credentials for both source and target registry in a single secret, the `.dockerconfigjson` file must contain multiple entries:

```json
{
  "auths": {
    "registry.redhat.io": {
      "auth": "<base64(user:password)>"
    },
    "quay.io": {
      "auth": "<base64(user:password)>"
    },
    "quay.example.com": {
      "auth": "<base64(user:password)>"
    }
  }
}
```

```bash
# Create a secret from an existing config.json
kubectl create secret generic all-creds \
  --from-file=.dockerconfigjson=/path/to/combined-config.json \
  --type=kubernetes.io/dockerconfigjson \
  -n mein-mirror
```

This secret is then set as `authSecret` in the `MirrorTarget` and passed to all manager and worker pods.

---

## 6. ImageSet Configuration

### 6.1 OpenShift Releases

#### Simple channel with version range

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
          maxVersion: "4.14.35"
```

#### Multiple channels and architectures

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
        - arm64
      channels:
        - name: stable-4.14
          minVersion: "4.14.28"
        - name: stable-4.15
          minVersion: "4.15.10"
          maxVersion: "4.15.20"
```

#### Shortest upgrade path between two versions

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
      channels:
        - name: stable-4.14
          minVersion: "4.14.10"
          maxVersion: "4.14.35"
          shortestPath: true  # only versions along the shortest upgrade path
```

#### Mirror the entire channel

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
      channels:
        - name: stable-4.14
          full: true  # all versions in the channel, from start to end
```

#### OKD

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
      channels:
        - name: stable-4.14
          type: okd
```

#### KubeVirt Container-Disk

```yaml
spec:
  mirror:
    platform:
      architectures:
        - amd64
      kubeVirtContainer: true  # RHCOS/FCOS disk images per architecture
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
```

### 6.2 Operator Catalogs

> **Heads-only default (oc-mirror v2 compatible):** When a package is listed
> without explicit channels or version ranges, only the **channel head** (latest
> version) of every channel is mirrored. This matches `oc-mirror v2` behaviour
> and keeps the mirror footprint small. To include more versions, use the
> `previousVersions` field or specify channels explicitly.

#### Single operator from a catalog

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
            # Heads-only: mirrors the latest version of every channel
```

#### Multiple packages from the same catalog

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
          - name: devworkspace-operator
          - name: openshift-pipelines-operator-rh
```

#### Head+N mode (include previous versions)

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
            previousVersions: 2   # head + 2 older versions per channel
```

> `previousVersions` only applies in heads-only mode — when no explicit
> `channels`, `minVersion` or `maxVersion` are specified for the package.

#### Specific channel of an operator

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: advanced-cluster-management
            channels:
              - name: release-2.9
```

#### Version range within a package

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: quay-operator
            minVersion: "3.8.0"
            maxVersion: "3.10.0"
```

#### Mirror the entire catalog

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        full: true  # all packages in the catalog
```

#### Disable dependency resolution

By default, the operator automatically resolves transitive dependencies (e.g., `odf-operator` → `odf-dependencies`). To disable this:

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        skipDependencies: true
        packages:
          - name: local-storage-operator
```

#### Rename the target catalog

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        targetCatalog: mein-mirror/mein-operator-katalog  # relative path in the target registry
        targetTag: "4.14"
        packages:
          - name: web-terminal
```

#### Multiple catalogs

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
      - catalog: registry.redhat.io/redhat/certified-operator-index:v4.14
        packages:
          - name: gpu-operator-certified
```

### 6.3 Additional Images

```yaml
spec:
  mirror:
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
      - name: registry.redhat.io/ubi9/ubi-minimal:9.3
      - name: quay.io/prometheus/prometheus:v2.48.0
        targetTag: v2.48.0-mirror      # optional target tag
      - name: docker.io/library/nginx:1.25
        targetRepo: mein-mirror/nginx  # optional target path
```

### 6.4 Splitting ImageSets (recommended)

For large deployments, it is recommended to split releases and operator catalogs into separate `ImageSet` objects:

```yaml
# imageset-releases.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: releases-4-14
  namespace: mein-mirror
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
---
# imageset-operators.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: operators-4-14
  namespace: mein-mirror
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
          - name: openshift-pipelines-operator-rh
```

Both `ImageSets` are referenced in the `MirrorTarget`:

```yaml
spec:
  imageSets:
    - releases-4-14
    - operators-4-14
```

---

## 7. MirrorTarget Configuration

### 7.1 Basic Configuration

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: mein-mirror
  namespace: mein-mirror
spec:
  registry: quay.example.com/mirror       # target registry (without https://)
  authSecret: mirror-creds                # secret name with registry credentials
  insecure: false                         # true = TLS fallback: tries HTTPS without verification first, falls back to plain HTTP
  imageSets:
    - releases-4-14
    - operators-4-14
```

### 7.2 Performance Tuning

```yaml
spec:
  # Maximum number of concurrently running worker pods (default: 1)
  # Higher values speed up mirroring but may trigger registry rate limits
  concurrency: 5

  # Number of images per worker pod (default: 50)
  # Higher values reduce pod start overhead, but on failure the
  # entire batch is retried
  batchSize: 50
```

> **Note on Quay:** When using Quay as the target registry, `concurrency: 1` is recommended. Quay's storage backend can produce digest mismatches on parallel uploads of the same blob layer. With `concurrency: 1`, later images benefit from the blob-mount mechanism (zero-copy), which still increases overall throughput.

### 7.3 Polling and CheckExist Intervals

```yaml
spec:
  # How often the manager checks upstream sources for new content
  # (new releases, new operator versions)
  # Default: 24h | Minimum: 1h | "0s" = disabled
  pollInterval: 24h

  # How often the manager checks whether images are still present in the target registry
  # On manager startup, the check always runs immediately (regardless of the interval)
  # Default: 6h | Minimum: 1h
  checkExistInterval: 6h
```

**What happens during the CheckExist check:**
- **Mirrored images:** If an image has been deleted from the target registry, it will be automatically re-mirrored (drift detection)
- **Permanently failed images** (`permanentlyFailed=true`): If the image is not yet in the target registry, a new mirroring attempt is started (handles transient upstream failures)

### 7.4 Expose (Resource Server)

The Resource Server provides IDMS, ITMS, CatalogSource, and other resources via HTTP.

#### OpenShift Route (default on OpenShift)

```yaml
spec:
  expose:
    type: Route
    host: mirror.apps.cluster.example.com  # optional, assigned automatically if omitted
```

#### Kubernetes Ingress

```yaml
spec:
  expose:
    type: Ingress
    host: mirror.example.com
    ingressClassName: nginx
```

#### Service only (no external access)

```yaml
spec:
  expose:
    type: Service
```

The Resource Server is then only accessible within the cluster at `http://<mirrortarget-name>-resources.<namespace>.svc.cluster.local:8081`.

#### Gateway API

```yaml
spec:
  expose:
    type: GatewayAPI
    gatewayRef:
      name: mein-gateway
      namespace: gateway-namespace
```

> **Note:** GatewayAPI support is still under development.

### 7.5 Pod Resources and Placement

```yaml
spec:
  manager:
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
    nodeSelector:
      kubernetes.io/os: linux
    tolerations:
      - key: "node-role.kubernetes.io/infra"
        operator: "Exists"
        effect: "NoSchedule"

  worker:
    resources:
      requests:
        cpu: "200m"
        memory: "256Mi"
      limits:
        cpu: "1"
        memory: "1Gi"
```

### 7.6 Image Cleanup on Removal

When an `ImageSet` is removed from `spec.imageSets`, all images of that `ImageSet` are **not** deleted from the target registry by default. To enable automatic deletion, an annotation can be set on the `MirrorTarget`:

```bash
kubectl annotate mirrortarget mein-mirror \
  mirror.openshift.io/cleanup-policy=Delete \
  -n mein-mirror
```

With this annotation, when an `ImageSet` is removed from the `spec.imageSets` list, all associated images will be deleted from the target registry via a cleanup job.

---

### 7.7 HTTP Proxy Configuration

If manager, worker, and catalog-builder pods must reach registries through a corporate HTTP proxy:

```yaml
spec:
  proxy:
    httpProxy: "http://proxy.corp.example.com:3128"
    httpsProxy: "http://proxy.corp.example.com:3128"
    # Optional: additional NO_PROXY entries (e.g. pod CIDR, custom domains)
    # noProxy: "10.128.0.0/14,.internal.corp.example.com"
```

**Auto-injected NO_PROXY entries:**

When `httpProxy` or `httpsProxy` is set, the operator automatically prepends the following to `NO_PROXY` in every pod:

```
localhost,127.0.0.1,.svc,.svc.cluster.local
```

This covers:
- Pod-to-pod traffic via the manager service (`<target>-manager.<namespace>.svc.cluster.local`)
- All other cluster-internal service FQDNs

**Kubernetes API access:**

The operator also overrides `KUBERNETES_SERVICE_HOST` to `kubernetes.default.svc.cluster.local`. This forces `client-go` to use the FQDN instead of the ClusterIP (which Kubernetes injects automatically and would not match FQDN-based NO_PROXY patterns). The Kubernetes API server certificate always includes `kubernetes.default.svc.cluster.local` as a SAN, so TLS verification is unaffected.

**The `spec.proxy.noProxy` field** is only needed for additional exclusions that go beyond the auto-injected defaults, for example:
- Pod or service CIDR (if the proxy intercepts intra-cluster IP traffic)
- Custom internal domains not covered by `.svc.cluster.local`

**Example with custom exclusions:**

```yaml
spec:
  proxy:
    httpProxy: "http://proxy.corp.example.com:3128"
    httpsProxy: "http://proxy.corp.example.com:3128"
    noProxy: "10.128.0.0/14,.corp.internal,my-registry.internal.example.com"
```

Both uppercase (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`) and lowercase (`http_proxy`, `https_proxy`, `no_proxy`) variants are injected for maximum compatibility.

---

### 7.8 Custom CA Bundle

To trust a custom or private CA when connecting to registries, create a ConfigMap with the PEM-encoded CA bundle:

```bash
kubectl create configmap custom-ca \
  --from-file=ca-bundle.crt=/path/to/ca-chain.pem \
  -n <namespace>
```

```yaml
spec:
  caBundle:
    configMapName: custom-ca
    key: ca-bundle.crt   # optional, defaults to "ca-bundle.crt"
```

The bundle is mounted into all manager, worker, and catalog-builder pods and `SSL_CERT_FILE` is set automatically.

---

### 7.9 Worker Storage for Large Images

By default, worker pods use a 10 GiB `emptyDir` volume as a blob buffer. For very large images (e.g. AI/ML models, LLMs) that exceed this limit, replace it with a dynamically-provisioned PVC:

```yaml
spec:
  workerStorage:
    size: "100Gi"
    storageClassName: "fast-ssd"   # optional, uses cluster default if omitted
```

The PVC uses a **generic ephemeral volume** — it is automatically deleted when the worker pod finishes. No manual cleanup is needed.

> **Note:** Blobs are always deleted from the buffer after upload. The volume only needs to be large enough for the **largest individual blob**, not the sum of all blobs in a batch.

---

## 8. Monitoring Status

### 8.1 MirrorTarget Status

```bash
kubectl get mirrortarget -n mein-mirror

# NAME          TOTAL   MIRRORED   PENDING   FAILED   AGE
# quay-mirror   4521    4519       0         2        2d
```

Detailed status (MirrorTarget):

```bash
kubectl get mirrortarget quay-mirror -n mein-mirror -o json | jq '.status'
```

```json
{
  "conditions": [
    {
      "type": "Ready",
      "status": "True",
      "reason": "Reconciled",
      "message": "MirrorTarget reconciled successfully",
      "lastTransitionTime": "2026-04-22T20:00:00Z"
    }
  ],
  "totalImages": 4521,
  "mirroredImages": 4519,
  "pendingImages": 0,
  "failedImages": 2,
  "imageSetStatuses": [
    {
      "name": "releases-4-14",
      "found": true,
      "total": 192,
      "mirrored": 192,
      "pending": 0,
      "failed": 0
    },
    {
      "name": "operators-4-14",
      "found": true,
      "total": 4329,
      "mirrored": 4327,
      "pending": 0,
      "failed": 2
    }
  ]
}
```

### 8.2 ImageSet Status

```bash
kubectl get imageset -n mein-mirror

# NAME              TOTAL   MIRRORED   PENDING   FAILED   AGE
# releases-4-14     192     192        0         0        2d
# operators-4-14    4329    4327       0         2        2d
```

Detailed status (ImageSet):

```bash
kubectl get imageset operators-4-14 -n mein-mirror -o json | jq '.status'
```

**Conditions:**

| Condition Type | Status | Reason | Meaning |
|---------------|--------|--------|---------|
| `Ready` | `True` | `Collected` | All images resolved, mirroring is running or complete |
| `Ready` | `False` | `Empty` | No images resolved yet (initial startup) |
| `CatalogReady` | `True` | `CatalogBuilt` | Filtered operator catalog successfully built and pushed |
| `CatalogReady` | `False` | `WaitingForOperatorMirror` | Waiting for operator mirroring to complete |
| `CatalogReady` | `False` | `NoCatalogConfigured` | No operator catalog configured in the spec |

### 8.3 Failed Images

After 10 failed attempts, an image is marked as `permanentlyFailed` and appears in `status.failedImageDetails`:

```bash
kubectl get imageset operators-4-14 -n mein-mirror -o json | jq '.status.failedImageDetails'
```

```json
[
  {
    "source": "registry.redhat.io/openshift4/ose-kube-rbac-proxy@sha256:fde63...",
    "destination": "quay.example.com/mirror/openshift4/ose-kube-rbac-proxy:sha256-fde63...",
    "error": "failed to copy image: MANIFEST_UNKNOWN: manifest unknown",
    "origin": "registry.redhat.io/redhat/redhat-operator-index:v4.14 [web-terminal, devworkspace-operator]"
  }
]
```

The `origin` field shows which catalog and which packages the image came from. This information can be used to notify the vendor about missing images.

> **Note:** `failedImageDetails` lists a maximum of 20 entries. The `failedImages` counter always reflects the correct total count.

---

## 9. Operations and Maintenance

### 9.1 Recollect (Force Re-sync)

The `recollect` trigger forces the manager to re-resolve all upstream sources — regardless of the configured `pollInterval` and regardless of cached digests. All permanently failed images are reset to `Pending` and retried.

```bash
kubectl annotate imageset operators-4-14 \
  mirror.openshift.io/recollect=$(date +%s) \
  --overwrite \
  -n mein-mirror
```

The annotation is automatically removed by the manager after completion (one-shot trigger).

**When recollect is useful:**
- After correcting invalid registry credentials
- When you are certain that a previously missing upstream image is now available
- After major spec changes (new packages, new channels)
- After registry issues (e.g., temporary outage of the source registry)

### 9.2 Cleanup (Delete Images)

**Remove a single ImageSet with cleanup:**

```bash
# 1. Set cleanup policy on MirrorTarget
kubectl annotate mirrortarget quay-mirror \
  mirror.openshift.io/cleanup-policy=Delete \
  -n mein-mirror

# 2. Remove ImageSet from the imageSets list
kubectl patch mirrortarget quay-mirror -n mein-mirror \
  --type=json \
  -p='[{"op":"remove","path":"/spec/imageSets/0"}]'
```

The cleanup job is started automatically and deletes all images that belonged exclusively to the removed `ImageSet`.

**Monitor the cleanup status:**

```bash
kubectl get mirrortarget quay-mirror -n mein-mirror \
  -o jsonpath='{.status.pendingCleanup}'
```

### 9.3 ImageSet Changes

When the spec of an `ImageSet` is modified (e.g., a new operator added, a package removed, version range adjusted), the following happens automatically:

1. The manager detects the spec change on the next reconcile cycle
2. New images are added as `Pending` and mirrored
3. Images that are no longer needed are removed from the state
4. If cleanup policy is active: images that are no longer needed are deleted from the registry

**Important:** A spec change (signature change) also resets permanently failed images back to `Pending` so they receive a new attempt.

### 9.4 Image Retries and Permanent Failures

#### Retry Mechanism

| Phase | Behavior |
|-------|----------|
| First attempt | Worker tries to mirror the image |
| Retry 1 | Worker waits 15s and retries (within the same worker pod) |
| `retryCount < 10` | Manager resets the image to `Pending` → new worker pod on next reconcile |
| `retryCount = 10` | Image is marked as `permanentlyFailed` |
| CheckExist interval | Manager checks the target registry; if image is missing → reset `retryCount` to 0, new attempt |

#### What to do with permanently failed images?

1. **Check the error message:**
   ```bash
   kubectl get imageset <name> -n <namespace> \
     -o json | jq '.status.failedImageDetails'
   ```

2. **Typical error causes:**
   | Error Message | Cause | Solution |
   |--------------|-------|---------|
   | `MANIFEST_UNKNOWN` | Image does not exist in the source registry | Check upstream; contact operator vendor (via `origin` field) |
   | `unauthorized` | Wrong credentials | Check and update `authSecret`, then recollect |
   | `connection refused` | Network issue | Check network/firewall |
   | `failed to send blob post` | Push error to target registry | Check target registry credentials |
   | `context deadline exceeded` | Timeout | Increase resources; reduce `batchSize` |

3. **Trigger a retry after resolving the issue:**
   ```bash
   kubectl annotate imageset <name> \
     mirror.openshift.io/recollect=$(date +%s) \
     --overwrite -n <namespace>
   ```

4. **Remove image from spec** (if permanently unavailable):
   ```yaml
   # Remove package from spec.mirror.operators[].packages
   # or delete the entire operator entry
   ```

---

## 10. Resource Server

The Resource Server runs in the manager pod on port 8081 and provides Kubernetes resources needed for configuring the mirrored cluster.

**Determine the base URL:**

```bash
# OpenShift Route
MIRROR_URL=$(kubectl get route <mirrortarget-name>-resources \
  -n <namespace> -o jsonpath='{.spec.host}')
echo "http://${MIRROR_URL}"

# Kubernetes Ingress
MIRROR_URL=$(kubectl get ingress <mirrortarget-name>-resources \
  -n <namespace> -o jsonpath='{.spec.rules[0].host}')

# Service (cluster-internal)
MIRROR_URL="<mirrortarget-name>-resources.<namespace>.svc.cluster.local:8081"
```

### 10.1 Retrieving IDMS and ITMS

```bash
# ImageDigestMirrorSet (for digest-based image refs)
curl http://${MIRROR_URL}/resources/<imageset-name>/idms.yaml | kubectl apply -f -

# ImageTagMirrorSet (for tag-based image refs)
curl http://${MIRROR_URL}/resources/<imageset-name>/itms.yaml | kubectl apply -f -

# List all available resources
curl http://${MIRROR_URL}/resources/
```

### 10.2 CatalogSource and ClusterCatalog

After a successful catalog build, the generated OLM resources are available:

```bash
# OLM v0: CatalogSource
curl http://${MIRROR_URL}/resources/<imageset-name>/catalogs/<catalog-name>/catalogsource.yaml \
  | kubectl apply -f -

# OLM v1: ClusterCatalog
curl http://${MIRROR_URL}/resources/<imageset-name>/catalogs/<catalog-name>/clustercatalog.yaml \
  | kubectl apply -f -
```

These resources reference the mirrored, filtered catalog image in the target registry.

### 10.3 Catalog Packages

Browse the packages, channels, and versions available in a mirrored catalog:

```bash
curl http://${MIRROR_URL}/resources/<imageset-name>/catalogs/<catalog-name>/packages.json | jq .
```

> The endpoint returns HTTP 409 if the catalog has not been built yet
> (`CatalogReady` condition not met).

### 10.4 Release Signatures

```bash
# ConfigMap with release signatures for the mirrored OCP release
curl http://${MIRROR_URL}/resources/<imageset-name>/signature-configmaps.yaml \
  | kubectl apply -f -
```

---

## 11. Full Configuration Reference

### 11.1 ImageSet Fields

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: <name>
  namespace: <namespace>
  annotations:
    # One-shot: forces the manager to fully re-resolve all upstream sources
    # Annotation is automatically removed after completion
    mirror.openshift.io/recollect: "1"
spec:
  mirror:
    platform:
      # Architectures: amd64, arm64, s390x, ppc64le
      architectures: [amd64]

      # Channels with optional version filters
      channels:
        - name: stable-4.14           # channel name (required)
          type: ocp                   # ocp (default) or okd
          minVersion: "4.14.10"       # lower bound (optional)
          maxVersion: "4.14.35"       # upper bound (optional)
          shortestPath: false         # shortest upgrade path
          full: false                 # mirror entire channel

      # Extract KubeVirt container disk images (RHCOS)
      kubeVirtContainer: false

    operators:
      - catalog: <catalog-image>      # catalog image (required)
        full: false                   # mirror entire catalog
        skipDependencies: false       # disable dependency resolution
        targetCatalog: <path>         # target path in the registry (optional)
        targetTag: <tag>              # target tag of the catalog image (optional)
        packages:
          - name: <package-name>      # package name (required)
            defaultChannel: <channel> # default channel (optional)
            previousVersions: 0       # older versions behind head to include (heads-only mode)
            minVersion: <version>     # minimum version (optional)
            maxVersion: <version>     # maximum version (optional)
            channels:
              - name: <channel>       # specific channel
                minVersion: <version>
                maxVersion: <version>

    additionalImages:
      - name: <image-ref>             # full image reference (required)
        targetRepo: <path>            # target repository path (optional)
        targetTag: <tag>              # target tag (optional)
```

**ImageSet Status:**

| Field | Type | Meaning |
|-------|------|---------|
| `conditions[].type=Ready` | Condition | Overall status of the ImageSet |
| `conditions[].type=CatalogReady` | Condition | Status of the filtered operator catalog |
| `totalImages` | int | Total number of resolved images |
| `mirroredImages` | int | Successfully mirrored images |
| `pendingImages` | int | Images still pending (including active workers) |
| `failedImages` | int | Permanently failed images |
| `failedImageDetails[]` | list | Details of failed images (max. 20) |
| `failedImageDetails[].source` | string | Source image reference |
| `failedImageDetails[].destination` | string | Destination image reference |
| `failedImageDetails[].error` | string | Last error message |
| `failedImageDetails[].origin` | string | Which spec entry produced this image |
| `lastSuccessfulPollTime` | time | Time of the last successful upstream poll |
| `observedGeneration` | int | Spec generation last reconciled |

### 11.2 MirrorTarget Fields

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: <name>
  namespace: <namespace>
  annotations:
    # Cleanup behavior when removing an ImageSet:
    # "Delete" = delete images from the registry (default: no deletion)
    mirror.openshift.io/cleanup-policy: "Delete"
spec:
  registry: <registry-url>            # target registry (required, without https://)
  authSecret: <secret-name>           # secret with registry credentials (optional)
  insecure: false                     # allow self-signed TLS

  imageSets:                          # list of referenced ImageSet names
    - <imageset-name>

  concurrency: 1                      # max. parallel worker pods (1–100, default: 1)
  batchSize: 50                       # images per worker pod (1–100, default: 50)

  pollInterval: 24h                   # upstream polling interval (min: 1h, "0s": off)
  checkExistInterval: 6h              # registry verification interval (min: 1h)

  expose:                             # Resource Server exposition
    type: Route                       # Route | Ingress | GatewayAPI | Service
    host: <hostname>                  # external hostname (optional for Route)
    ingressClassName: <class>         # only for type=Ingress
    gatewayRef:                       # only for type=GatewayAPI
      name: <gateway-name>
      namespace: <namespace>

  manager:                            # manager pod configuration
    resources: {}                     # standard Kubernetes ResourceRequirements
    nodeSelector: {}                  # node selector
    tolerations: []                   # tolerations

  worker:                             # worker pod configuration
    resources: {}
    nodeSelector: {}
    tolerations: []

  proxy:                              # HTTP proxy configuration (optional)
    httpProxy: <url>                  # HTTP_PROXY value
    httpsProxy: <url>                 # HTTPS_PROXY value
    noProxy: <csv>                    # additional NO_PROXY entries (cluster-internal defaults are auto-injected)

  caBundle:                           # custom CA bundle (optional)
    configMapName: <name>             # ConfigMap containing the PEM bundle
    key: ca-bundle.crt                # key in the ConfigMap (default: "ca-bundle.crt")

  workerStorage:                      # ephemeral PVC for large image blobs (optional)
    size: "10Gi"                      # PVC size (default: "10Gi")
    storageClassName: <class>         # StorageClass (default: cluster default)
```

**MirrorTarget Status:**

| Field | Type | Meaning |
|-------|------|---------|
| `conditions[].type=Ready` | Condition | Overall status of the MirrorTarget |
| `totalImages` | int | Total across all ImageSets |
| `mirroredImages` | int | Successfully mirrored across all ImageSets |
| `pendingImages` | int | Pending across all ImageSets |
| `failedImages` | int | Permanently failed across all ImageSets |
| `imageSetStatuses[]` | list | Per-ImageSet breakdown |
| `imageSetStatuses[].found` | bool | false if ImageSet does not exist |
| `pendingCleanup[]` | list | ImageSets whose cleanup is running |

---

## 12. Examples

### Minimal Production Configuration

```yaml
# Namespace
apiVersion: v1
kind: Namespace
metadata:
  name: ocp-mirror
---
# Registry credentials
apiVersion: v1
kind: Secret
metadata:
  name: registry-creds
  namespace: ocp-mirror
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: <base64-encoded-docker-config>
---
# OpenShift 4.14 Releases
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-14-releases
  namespace: ocp-mirror
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
---
# Operator catalog
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-14-operators
  namespace: ocp-mirror
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
          - name: openshift-pipelines-operator-rh
---
# MirrorTarget
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: intern-quay
  namespace: ocp-mirror
spec:
  registry: quay.intern.example.com/ocp-mirror
  authSecret: registry-creds
  imageSets:
    - ocp-4-14-releases
    - ocp-4-14-operators
  concurrency: 3
  batchSize: 50
  pollInterval: 24h
  expose:
    type: Route
```

### Air-Gap Staging with Multiple Versions

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: multi-version-releases
  namespace: ocp-mirror
spec:
  mirror:
    platform:
      architectures:
        - amd64
        - arm64
      channels:
        - name: stable-4.14
          minVersion: "4.14.28"
          maxVersion: "4.14.35"
        - name: stable-4.15
          minVersion: "4.15.10"
          maxVersion: "4.15.25"
        - name: stable-4.16
          minVersion: "4.16.0"
```

### Full Multi-Catalog Configuration

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: alle-kataloge
  namespace: ocp-mirror
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: advanced-cluster-management
            channels:
              - name: release-2.9
          - name: quay-operator
          - name: openshift-gitops-operator
      - catalog: registry.redhat.io/redhat/certified-operator-index:v4.14
        packages:
          - name: gpu-operator-certified
      - catalog: registry.redhat.io/redhat/community-operator-index:v4.14
        packages:
          - name: argocd-operator
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
      - name: registry.redhat.io/ubi9/ubi-minimal:latest
```

---

## 13. Troubleshooting

### Manager does not start

```bash
# Check controller logs
kubectl logs deployment/oc-mirror-operator-controller-manager \
  -n oc-mirror-operator -f

# Check events in the namespace
kubectl get events -n mein-mirror --sort-by='.lastTimestamp'
```

**Common issues:**

| Symptom | Cause | Solution |
|---------|-------|---------|
| `OPERATOR_IMAGE not set` | Missing env var in the controller deployment | Re-apply the CSV via OLM |
| Manager pod does not start (ImagePullBackOff) | Wrong image reference | Check the digest in the MirrorTarget deployment |
| `MountVolume.SetUp failed: references non-existent secret key` | Secret has wrong key | Secret must have a `.dockerconfigjson` key (type `kubernetes.io/dockerconfigjson`) |

### Worker pods get stuck

```bash
# Read worker logs
kubectl logs <worker-pod-name> -n mein-mirror

# All worker events
kubectl get events -n mein-mirror \
  --field-selector reason=Failed --sort-by='.lastTimestamp'
```

### Images remain permanently on Pending

```bash
# Check manager logs for errors
kubectl logs deployment/<mirrortarget>-manager -n mein-mirror | grep -i error

# Inspect image state directly in the ConfigMap
kubectl get configmap <imageset-name>-images -n mein-mirror -o json | \
  python3 -c "
import sys, json, gzip, base64
d = json.load(sys.stdin)
state = json.loads(gzip.decompress(base64.b64decode(d['binaryData']['state'])))
failed = {k:v for k,v in state.items() if v['state']=='Failed'}
print(json.dumps(failed, indent=2))
"
```

### Registry Authentication Errors

```bash
# Mit skopeo testen ob das Secret funktioniert
oc run skopeo-test --rm -it \
  --image=quay.io/skopeo/stable:latest \
  --restart=Never \
  --overrides='{
    "spec": {
      "volumes": [{"name":"auth","secret":{"secretName":"mirror-creds"}}],
      "containers": [{
        "name":"skopeo",
        "image":"quay.io/skopeo/stable:latest",
        "command":["skopeo","inspect",
          "--authfile","/auth/.dockerconfigjson",
          "docker://registry.redhat.io/ubi9/ubi:latest"],
        "volumeMounts":[{"name":"auth","mountPath":"/auth"}]
      }]
    }
  }' -- /bin/sh
```

### CatalogReady remains False

```bash
# Check ImageSet conditions
kubectl get imageset <name> -n <namespace> -o json | \
  jq '.status.conditions[] | select(.type=="CatalogReady")'

# Check the catalog builder job
kubectl get jobs -n <namespace> -l imageset=<name>
kubectl logs job/<catalog-builder-job-name> -n <namespace>
```

If `CatalogReady=False` with reason `WaitingForOperatorMirror`:
- There are still pending or failed operator images
- Check and resolve failed images in `failedImageDetails`

### Quay-Specific Issues

**Symptom:** `failed to send blob post: unauthorized` even though credentials are correct

**Cause:** Quay creates new repositories only on the first push attempt. After that, the repository must either be set to "Public" in Quay, or the push user must be configured as a member of the organization with write permissions.

**Solution:** Create a robot account in Quay with `write` permission on the organization and use it as the `authSecret`.

---

### 13.5 HTTP Proxy Issues

**Symptom:** Manager pod cannot reach the Kubernetes API (connection refused/timeout via proxy)

**Cause:** The proxy is intercepting requests to the Kubernetes API ClusterIP. `client-go` uses the `KUBERNETES_SERVICE_HOST` environment variable which is auto-injected by Kubernetes as an IP address. IP addresses do not match FQDN-based `NO_PROXY` rules.

**Solution:** This is automatically fixed by the operator when `spec.proxy.httpProxy` or `spec.proxy.httpsProxy` is set: `KUBERNETES_SERVICE_HOST` is overridden to `kubernetes.default.svc.cluster.local` so that the `.svc.cluster.local` NO_PROXY rule applies.

If you are seeing this issue, verify that:
1. `spec.proxy.httpProxy` or `spec.proxy.httpsProxy` is set in your MirrorTarget
2. The controller pod itself also has proxy settings configured (this is done at the OLM/Subscription level, not via the MirrorTarget spec)

**Symptom:** Worker pods cannot reach the manager service

**Cause:** The manager service FQDN (`<target>-manager.<namespace>.svc.cluster.local`) is being sent through the proxy.

**Solution:** Automatically handled — `.svc.cluster.local` is always in `NO_PROXY` when a proxy is configured. Verify the manager deployment has the correct NO_PROXY env var:
```bash
kubectl get deployment <mirrortarget>-manager -n <namespace> \
  -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.[] | select(.name=="NO_PROXY")'
```

**Symptom:** Custom registry with self-signed certificate fails even with proxy configured

**Solution:** Use `spec.caBundle` in addition to `spec.proxy` to trust the registry's CA:
```yaml
spec:
  proxy:
    httpProxy: "http://proxy.corp.example.com:3128"
  caBundle:
    configMapName: my-registry-ca
```

---

**Further Reading:**
- [Architecture and Developer Documentation](../README.md)
- [GitHub Repository](https://github.com/mariusbertram/oc-mirror-operator)
- [Issues and Feature Requests](https://github.com/mariusbertram/oc-mirror-operator/issues)
