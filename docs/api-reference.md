# API Reference

> API Group: `mirror.openshift.io/v1alpha1`

---

## MirrorTarget

`MirrorTarget` is the primary resource that represents a target registry and the
set of ImageSets whose content should be mirrored to it.

```
kubectl get mirrortargets -n <namespace>
```

### Spec

| Field | Type | Required | Description |
|---|---|---|---|
| `registry` | `string` | **yes** | URL of the target registry, e.g. `registry.example.com/mirror`. |
| `imageSets` | `[]string` | no | Names of ImageSet objects (same namespace) to mirror to this target. One ImageSet may only be referenced by a single MirrorTarget. |
| `insecure` | `bool` | no | Allow plaintext HTTP or self-signed TLS for the target registry. |
| `authSecret` | `string` | no | Name of a Secret containing registry credentials. Supports `username`/`password` keys or `.dockerconfigjson`. |
| `concurrency` | `int` | no | Maximum number of worker pods running in parallel. Range: 1–100. Default: `20`. |
| `batchSize` | `int` | no | Number of images per worker pod. Range: 1–100. Default: `10`. |
| `pollInterval` | `duration` | no | How often to re-check upstream sources for new content. Minimum `1h`. Set `0s` to disable. Default: `24h`. |
| `checkExistInterval` | `duration` | no | How often to verify mirrored images still exist in the target. Minimum `1h`. Default: `6h`. Also triggers retry for permanently-failed images. |
| `expose` | `ExposeConfig` | no | How the resource server HTTP endpoint is exposed. Auto-creates a Route on OpenShift if omitted. |
| `manager` | `PodConfig` | no | Resource requests/limits, node selector, and tolerations for the manager pod. |
| `worker` | `PodConfig` | no | Resource requests/limits, node selector, and tolerations for worker pods. |
| `proxy` | `ProxyConfig` | no | HTTP/HTTPS proxy for all pods. See [ProxyConfig](#proxyconfigspec). |
| `caBundle` | `CABundleRef` | no | Custom CA bundle ConfigMap reference. See [CABundleRef](#cabundleref). |
| `workerStorage` | `WorkerStorageConfig` | no | Ephemeral PVC for worker blob buffer. See [WorkerStorageConfig](#workerstorageconfig). |

#### Annotations

| Annotation | Value | Description |
|---|---|---|
| `mirror.openshift.io/cleanup-policy` | `Delete` | Delete images from the target registry when an ImageSet is removed from `spec.imageSets`. |
| `mirror.openshift.io/recollect` | `"true"` (any value) | Force re-resolution of all upstream content on the next reconcile. One-shot: removed by the manager after recollection completes. |

---

### ProxyConfig

Configures HTTP/HTTPS proxy settings injected into worker, manager, and
catalog-builder pods.

> **Auto-injected NO_PROXY**: When any proxy field is set, the operator
> automatically prepends `localhost,127.0.0.1,.svc,.svc.cluster.local` to
> `NO_PROXY` so in-cluster traffic always bypasses the proxy.
>
> **KUBERNETES_SERVICE_HOST override**: When any proxy field is set,
> `KUBERNETES_SERVICE_HOST` is overridden to
> `kubernetes.default.svc.cluster.local` so that `client-go` uses the FQDN
> (already in NO_PROXY) rather than the bare ClusterIP.

| Field | Type | Description |
|---|---|---|
| `httpProxy` | `string` | Sets `HTTP_PROXY` / `http_proxy`. |
| `httpsProxy` | `string` | Sets `HTTPS_PROXY` / `https_proxy`. |
| `noProxy` | `string` | Additional NO_PROXY entries (comma-separated) appended **after** the auto-injected cluster defaults. |

**Example**:
```yaml
spec:
  proxy:
    httpsProxy: "https://proxy.corp.example.com:3128"
    noProxy: "192.168.0.0/16,my-internal-registry.corp.example.com"
```

---

### CABundleRef

References a ConfigMap in the same namespace containing a PEM CA bundle.

| Field | Type | Required | Description |
|---|---|---|---|
| `configMapName` | `string` | **yes** | Name of the ConfigMap. |
| `key` | `string` | no | Key inside the ConfigMap. Default: `ca-bundle.crt`. |

The bundle is mounted into all pods at `/etc/ssl/certs/ca-bundle.crt` and
`SSL_CERT_FILE` is set to point to it.

**Example**:
```yaml
spec:
  caBundle:
    configMapName: corporate-ca
    key: ca.pem
```

---

### WorkerStorageConfig

Replaces the default `emptyDir` blob buffer with a dynamically-provisioned
generic ephemeral PVC per worker pod.

| Field | Type | Description |
|---|---|---|
| `storageClassName` | `string` | StorageClass to use. Defaults to the cluster default. |
| `size` | `resource.Quantity` | Requested capacity. Default: `10Gi`. |

Blob files are deleted immediately after a successful upload to the target
registry, so the volume only needs to hold the largest single blob at a time —
not the sum of all blobs in a batch.

**Example**:
```yaml
spec:
  workerStorage:
    storageClassName: fast-nvme
    size: 200Gi
```

---

### ExposeConfig

| Field | Type | Description |
|---|---|---|
| `type` | `Route \| Ingress \| GatewayAPI \| Service` | Exposure mechanism. Auto-detects: Route on OpenShift, Service otherwise. |
| `host` | `string` | External hostname. Auto-generated for Routes on OpenShift. |
| `ingressClassName` | `string` | Ingress controller class (type=Ingress only). |
| `gatewayRef.name` | `string` | Gateway resource name (type=GatewayAPI only). |
| `gatewayRef.namespace` | `string` | Gateway namespace (defaults to MirrorTarget namespace). |

---

### PodConfig

Applies to both `spec.manager` and `spec.worker`.

| Field | Type | Description |
|---|---|---|
| `resources` | `corev1.ResourceRequirements` | CPU/memory requests and limits. |
| `nodeSelector` | `map[string]string` | Node selector labels. |
| `tolerations` | `[]corev1.Toleration` | Pod tolerations. |

---

### Status

| Field | Type | Description |
|---|---|---|
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions. See [Conditions](#conditions). |
| `totalImages` | `int` | Cumulative image count across all ImageSets. |
| `mirroredImages` | `int` | Cumulative successfully mirrored count. |
| `pendingImages` | `int` | Cumulative pending/in-progress count. |
| `failedImages` | `int` | Cumulative permanently-failed count. |
| `imageSetStatuses` | `[]ImageSetStatusSummary` | Per-ImageSet breakdown. |
| `knownImageSets` | `[]string` | Last observed `spec.imageSets` list (internal). |
| `pendingCleanup` | `[]string` | ImageSets currently being cleaned up. |

#### ImageSetStatusSummary

| Field | Type | Description |
|---|---|---|
| `name` | `string` | ImageSet name. |
| `found` | `bool` | Whether the ImageSet exists in the namespace. |
| `total` | `int` | Total images in this ImageSet. |
| `mirrored` | `int` | Successfully mirrored. |
| `pending` | `int` | Pending or in-progress. |
| `failed` | `int` | Permanently failed. |

#### Conditions

| Type | Status | Reason | Description |
|---|---|---|---|
| `Ready` | `True` | `AllImagesMirrored` | All images are mirrored. |
| `Ready` | `False` | `MirroringInProgress` | Mirroring is in progress. |
| `Ready` | `False` | `ImagesFailed` | One or more images permanently failed. |
| `Ready` | `False` | `ManagerNotReady` | Manager pod is not yet running. |
| `CatalogReady` | `True` | `CatalogBuilt` | Filtered catalog image built successfully. |
| `CatalogReady` | `False` | `CatalogBuildInProgress` | Catalog build Job is running. |
| `CatalogReady` | `False` | `CatalogBuildFailed` | Catalog build Job failed. |

---

## ImageSet

`ImageSet` declares which content to mirror. It is bound to a target via
`MirrorTarget.spec.imageSets`. An ImageSet may only be referenced by one
MirrorTarget at a time.

```
kubectl get imagesets -n <namespace>
```

### Spec

| Field | Type | Description |
|---|---|---|
| `mirror` | `Mirror` | Content to mirror. See [Mirror](#mirror). |

---

### Mirror

| Field | Type | Description |
|---|---|---|
| `platform` | `Platform` | OpenShift/OKD release channels. |
| `operators` | `[]Operator` | OLM operator catalogs. |
| `additionalImages` | `[]AdditionalImage` | Individual images by reference. |
| `helm` | `Helm` | Helm chart images. |
| `blockedImages` | `[]BlockedImage` | Images to exclude from all other entries. |

#### Platform

| Field | Type | Description |
|---|---|---|
| `channels` | `[]ReleaseChannel` | Release channels to mirror. |
| `architectures` | `[]string` | Architectures to include (e.g. `amd64`, `arm64`). |
| `graph` | `bool` | Whether to include Cincinnati graph data. |
| `kubeVirtContainer` | `bool` | Extract the KubeVirt container from release payload. |

#### ReleaseChannel

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Channel name, e.g. `stable-4.14`. |
| `type` | `ocp \| okd` | Platform type. Default: `ocp`. |
| `minVersion` | `string` | Minimum version to mirror. |
| `maxVersion` | `string` | Maximum version to mirror. |
| `shortestPath` | `bool` | Mirror only the shortest upgrade path between min and max. |
| `full` | `bool` | Mirror all versions in the channel. |

#### Operator

| Field | Type | Description |
|---|---|---|
| `catalog` | `string` | Upstream catalog image (e.g. `registry.redhat.io/redhat/redhat-operator-index:v4.14`). Supports `image:tag`, `image:tag@sha256:digest`, and `image@sha256:digest` formats. |
| `packages` | `[]IncludePackage` | Packages to include. Omit to include all (equivalent to `full: true`). |
| `full` | `bool` | Explicitly include all packages. |
| `targetCatalog` | `string` | Override the full URL of the built catalog image. |
| `targetTag` | `string` | Override the tag for the built catalog image. |
| `skipDependencies` | `bool` | Do not resolve `olm.gvk.required` dependencies. |

#### IncludePackage

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Package name. |
| `channels` | `[]IncludeChannel` | Specific channels within this package. Omit for **heads-only** mode (see below). |
| `defaultChannel` | `string` | Override the default channel for this package. |
| `previousVersions` | `int` | Number of older versions behind the channel head to include in heads-only mode. Default: `0` (head only). Only effective when no explicit channels or version ranges are specified. |
| `minVersion` | `string` | Minimum bundle version to include. |
| `maxVersion` | `string` | Maximum bundle version to include. |

> **Heads-only mode (oc-mirror v2 compatible):** When a package is listed without
> explicit `channels`, `minVersion`, or `maxVersion`, only the **channel head**
> (latest version) of every channel is included. Set `previousVersions` to
> include additional older versions. To mirror all versions of specific channels,
> specify them explicitly in the `channels` array.

#### IncludeChannel

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Channel name. |
| `minVersion` | `string` | Minimum bundle version within this channel. |
| `maxVersion` | `string` | Maximum bundle version within this channel. |

#### AdditionalImage

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Full image reference (e.g. `quay.io/cert-manager/cert-manager-controller:v1.14.0`). |

---

### Status

| Field | Type | Description |
|---|---|---|
| `conditions` | `[]metav1.Condition` | Standard conditions (see above). |
| `totalImages` | `int` | Total image count for this ImageSet. |
| `mirroredImages` | `int` | Successfully mirrored. |
| `pendingImages` | `int` | Pending or in-progress. |
| `failedImages` | `int` | Permanently failed. |
| `failedImageDetails` | `[]FailedImageDetail` | Up to 20 failed image entries with source, destination, error, and origin. |
| `observedGeneration` | `int64` | Last reconciled spec generation. |
| `lastSuccessfulPollTime` | `time` | Timestamp of the last successful upstream poll. |

#### FailedImageDetail

| Field | Description |
|---|---|
| `source` | Upstream image reference. |
| `destination` | Target registry reference. |
| `error` | Last error message. |
| `origin` | Human-readable description of the spec entry that produced this image. |

---

## RBAC Resources

The operator creates the following per-MirrorTarget RBAC resources in the
MirrorTarget's namespace:

| Resource | Name | Purpose |
|---|---|---|
| `ServiceAccount` | `{mt.Name}-coordinator` | Identity for the manager pod. |
| `ServiceAccount` | `{mt.Name}-worker` | Identity for worker pods. |
| `Role` | `{mt.Name}-coordinator` | Grants manager: get/list/watch/create/update/patch/delete for Pods, ConfigMaps, Jobs, PVCs. |
| `Role` | `{mt.Name}-worker` | Grants workers: get/list/watch for Secrets and ConfigMaps. |
| `RoleBinding` | `{mt.Name}-coordinator` | Binds coordinator Role to coordinator SA. |
| `RoleBinding` | `{mt.Name}-worker` | Binds worker Role to worker SA. |

All resources are owned by the MirrorTarget and are garbage-collected when the
MirrorTarget is deleted.

> **Upgrade note (≤v0.0.5 → v0.0.6)**: Prior versions created fixed-name
> resources (`oc-mirror-coordinator`, `oc-mirror-worker`). These are orphaned
> after upgrading to v0.0.6 and must be deleted manually:
> ```
> kubectl delete sa,role,rolebinding oc-mirror-coordinator oc-mirror-worker -n <namespace>
> ```

---

## Full Example

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-mirror
  namespace: oc-mirror-operator
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.14
          minVersion: "4.14.30"
          maxVersion: "4.14.35"
          shortestPath: true
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.14
        packages:
          - name: web-terminal
          - name: cert-manager
            channels:
              - name: stable-v1
    additionalImages:
      - name: quay.io/jetstack/cert-manager-controller:v1.14.0
---
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-mirror
  namespace: oc-mirror-operator
spec:
  registry: registry.corp.example.com/mirror
  imageSets:
    - ocp-mirror
  authSecret: registry-credentials
  concurrency: 10
  batchSize: 5
  pollInterval: 12h
  proxy:
    httpsProxy: "https://proxy.corp.example.com:3128"
    noProxy: "192.168.0.0/16"
  caBundle:
    configMapName: corporate-ca
  workerStorage:
    size: 50Gi
  expose:
    type: Route
    host: mirror.apps.cluster.example.com
```
