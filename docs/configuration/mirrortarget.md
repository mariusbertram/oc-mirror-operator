# Configuring MirrorTargets

A `MirrorTarget` describes **where** content goes and **how** the mirroring runs. Every
field is optional except `registry`; the defaults are tuned for a Quay target with
moderate load.

**Contents**

- [Target registry](#target-registry)
- [ImageSets](#imagesets)
- [Throughput: concurrency and batch size](#throughput-concurrency-and-batch-size)
- [Intervals: polling and drift check](#intervals-polling-and-drift-check)
- [Cleanup policy](#cleanup-policy)
- [Pod resources and placement](#pod-resources-and-placement)
- [HTTP proxy](#http-proxy)
- [Custom CA bundle](#custom-ca-bundle)
- [Worker storage for large images](#worker-storage-for-large-images)
- [Exposing the Resource API](#exposing-the-resource-api)
- [Complete example](#complete-example)

---

## Target registry

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
  namespace: mirror
spec:
  registry: registry.example.com/mirror   # host[:port][/path], no scheme
  authSecret: registry-creds              # see credentials.md
  insecure: false
```

- `registry` is the prefix of every destination reference. A path component (`/mirror`)
  is recommended so the mirror lives in its own organization/project.
- `insecure: true` switches the registry client to *HTTP first, then HTTPS without
  certificate verification*. Use it for in-cluster `registry:5000`-style registries.
  For registries with a private CA prefer [`caBundle`](#custom-ca-bundle) and keep
  `insecure: false`.

## ImageSets

```yaml
spec:
  imageSets:
    - ocp-4-16-releases
    - ocp-4-16-operators
```

Names of `ImageSet`s in the same namespace. Adding a name starts mirroring; removing one
stops it and, with the [cleanup policy](#cleanup-policy), deletes its exclusive images.
An `ImageSet` may appear in only one `MirrorTarget`.

## Throughput: concurrency and batch size

```yaml
spec:
  concurrency: 1     # worker pods running at the same time (1–100, default 1)
  batchSize: 50      # images per worker pod (1–100, default 50)
```

- **Quay:** keep `concurrency: 1`. Quay's storage backend can corrupt concurrent uploads
  of the same blob into different repositories. Sequential workers still get most of the
  speed benefit because blobs already pushed by an earlier image are *mounted*
  (zero-copy) rather than re-uploaded, and the manager orders images so that shared
  layers come first.
- **Other registries:** raise `concurrency` until the source registry's rate limit or
  your bandwidth becomes the bottleneck. 3–5 is a good start.
- `batchSize` trades pod start-up overhead against the size of the unit that is retried
  when a pod dies. Each worker pod gets an `activeDeadlineSeconds` of 45 minutes ×
  `batchSize`.

## Intervals: polling and drift check

```yaml
spec:
  pollInterval: 24h          # upstream re-resolution; min 1h; "0s" disables
  checkExistInterval: 6h     # target registry verification; min 1h
```

| | Polling | Drift check |
|---|---|---|
| Looks at | Cincinnati graph, catalog image digests, Helm indexes | Every `Mirrored` image in the target registry |
| Finds | New releases, new bundle versions, republished catalog tags | Images deleted from the target, additional-image tags that moved upstream, missing cosign signatures (`requireSignedImages`) |
| Result | New images `Pending`, dropped images orphaned, catalog rebuild once mirrored | Missing images back to `Pending` and re-mirrored; permanently failed images retried if still absent |
| Runs | In the manager reconcile, per `ImageSet` | In the background, 20 checks in parallel, so dispatching continues |

`pollInterval: 0s` freezes the content at what is currently resolved; spec edits and the
`recollect` annotation still work.

## Retries

```yaml
spec:
  maxRetries: 10     # failed attempts per image before it is permanently failed (min 1)
```

A failed image is retried after a backoff of 1 minute, doubling with each failure up to
1 hour (±10 % jitter), so the default budget of 10 attempts spans several hours. While
an image waits for its next attempt it does not occupy a worker slot. Raise
`maxRetries` for flaky source registries; lower it to surface broken images sooner.

## Cleanup policy

```yaml
metadata:
  annotations:
    mirror.openshift.io/cleanup-policy: Delete
```

Opt-in. With the annotation, images that are no longer referenced by any `ImageSet` of
this target — because an `ImageSet` was removed from `spec.imageSets`, a package or
version range was narrowed, or an image was blocked — are deleted from the target
registry by a cleanup Job. Without it nothing is ever deleted. Details and caveats in
[Operations → Cleanup](../operations.md#cleanup).

## Pod resources and placement

```yaml
spec:
  manager:
    resources:
      requests: { cpu: 500m, memory: 512Mi }
      limits:   { cpu: "2",  memory: 2Gi }
    nodeSelector:
      node-role.kubernetes.io/infra: ""
    tolerations:
      - key: node-role.kubernetes.io/infra
        operator: Exists
        effect: NoSchedule
  worker:
    resources:
      requests: { cpu: 200m, memory: 256Mi }
      limits:   { cpu: "1",  memory: 1Gi }
```

`worker` also applies to cleanup Jobs. Memory needs of workers are small regardless of
image size because large layers are buffered on disk, not in RAM. The manager holds the
whole image state in memory; budget roughly 1 GiB per 100 000 images.

Worker pods carry a *preferred* pod anti-affinity against each other
(`topologyKey: kubernetes.io/hostname`, all `app=oc-mirror-worker` pods in the
namespace), so with `concurrency` > 1 the scheduler spreads them across nodes and
their network and disk load is shared. It is only a preference: with fewer
schedulable nodes than concurrent workers, workers share nodes rather than stay
`Pending`. `nodeSelector` and `tolerations` narrow the candidate nodes as usual.

## HTTP proxy

```yaml
spec:
  proxy:
    httpProxy:  http://proxy.corp.example.com:3128
    httpsProxy: http://proxy.corp.example.com:3128
    noProxy: 10.128.0.0/14,.corp.internal        # optional extras
```

Applies to manager, worker, catalog-build and cleanup pods. The operator injects both
upper- and lower-case variables and **always prepends**
`localhost,127.0.0.1,.svc,.svc.cluster.local` to `NO_PROXY`, so worker→manager traffic
stays in-cluster. It also sets `KUBERNETES_SERVICE_HOST=kubernetes.default.svc.cluster.local`
so client-go's API access matches the `.svc.cluster.local` exclusion. Add `noProxy`
entries only for things beyond that — pod/service CIDRs if the proxy intercepts IP
traffic, internal registries that should bypass the proxy.

The controller pod itself does not read `spec.proxy`; configure it through the
Subscription (`spec.config.env`) or the Deployment if it needs a proxy to reach the
cluster.

## Custom CA bundle

```bash
kubectl create configmap corporate-ca --from-file=ca-bundle.crt=/path/to/chain.pem -n mirror
```

```yaml
spec:
  caBundle:
    configMapName: corporate-ca
    key: ca-bundle.crt          # default
```

Mounted at `/run/secrets/ca/` in all pods with `SSL_CERT_FILE` pointing at it. Use this
for registries (and proxies) with a private CA instead of `insecure: true`.

## Worker storage for large images

Workers buffer layers above 100 MiB in `/tmp/blob-buffer` before uploading them, which
avoids Quay upload-session timeouts on slow cross-registry transfers. The default buffer
is a 10 GiB `emptyDir`. For images with individual layers larger than that (AI/ML models,
virtual machine images) switch to a generic ephemeral PVC:

```yaml
spec:
  workerStorage:
    size: 200Gi
    storageClassName: fast-ssd      # optional, cluster default otherwise
```

The PVC is created and deleted with each worker pod. It only needs to hold the
**largest single layer**, not a whole batch — buffered layers are removed right after
their upload.

## Exposing the Resource API

The Resource API (see [Consuming the mirror](../consuming-results.md)) runs as one
Deployment per namespace on port 8081. Each `MirrorTarget` gets a Service
`<name>-resources` pointing at it, and `spec.expose` decides how that Service is reached:

```yaml
spec:
  expose:
    type: Route            # Route | Ingress | GatewayAPI | Service
    host: mirror.apps.example.com      # optional
```

| `type` | Creates | Notes |
|---|---|---|
| `Route` | OpenShift Route `<name>-resources` with edge TLS | **Default on OpenShift** (auto-detected). `host` optional. |
| `Ingress` | `networking.k8s.io/v1` Ingress | Set `host`; `ingressClassName` optional. |
| `GatewayAPI` | `HTTPRoute` attached to `gatewayRef` | Requires the Gateway API CRDs and `gatewayRef.name`; `gatewayRef.namespace` defaults to the MirrorTarget's. `host` becomes the route's hostname. |
| `Service` | nothing beyond the Service | **Default on plain Kubernetes.** Reach it via port-forward or in-cluster at `http://oc-mirror-resource-api.<ns>.svc:8081`. |

Switching the type removes the objects of the previous type.

## Complete example

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
  namespace: mirror
  annotations:
    mirror.openshift.io/cleanup-policy: Delete
spec:
  registry: registry.example.com/mirror
  authSecret: registry-creds
  imageSets:
    - ocp-4-16-releases
    - ocp-4-16-operators
  concurrency: 1
  batchSize: 50
  pollInterval: 12h
  checkExistInterval: 6h
  maxRetries: 10
  expose:
    type: Route
  proxy:
    httpsProxy: http://proxy.corp.example.com:3128
  caBundle:
    configMapName: corporate-ca
  workerStorage:
    size: 50Gi
  manager:
    resources:
      requests: { cpu: 500m, memory: 1Gi }
  worker:
    resources:
      requests: { cpu: 200m, memory: 256Mi }
```

`registry`, `insecure`, `authSecret`, `concurrency`, `batchSize`, `pollInterval` and
`checkExistInterval` can also be edited from the console plugin's **Settings** tab.
