# Operations

Day-2 tasks: reading status, forcing re-resolution or re-transfer, dealing with failed
images, changing specs, cleaning up, and monitoring.

**Contents**

- [Reading status](#reading-status)
- [Failed images](#failed-images)
- [Recollect](#recollect)
- [Force resync](#force-resync)
- [Changing an ImageSet](#changing-an-imageset)
- [Cleanup](#cleanup)
- [Restarting components](#restarting-components)
- [Monitoring](#monitoring)
- [Known issues](#known-issues)

---

## Reading status

### At a glance

```bash
kubectl get mirrortarget,imageset -n mirror
# NAME                 TOTAL   MIRRORED   PENDING   FAILED   AGE
# internal-registry    4521    4519       0         2        2d
# ocp-4-16-releases    192     192        0         0        2d
# ocp-4-16-operators   4329    4327       0         2        2d
```

`TOTAL` counts distinct destinations; `PENDING` includes images currently being copied;
`FAILED` counts permanently failed images only. The `MirrorTarget` numbers are
deduplicated across its `ImageSet`s.

### Conditions

```bash
kubectl get imageset ocp-4-16-operators -n mirror -o jsonpath='{.status.conditions}' | jq
```

| Resource | Condition | Reason | Meaning |
|---|---|---|---|
| ImageSet | `Ready=True` | `Collected` | Resolved; mirroring in progress or complete (message has the counts). |
| ImageSet | `Ready=False` | `Empty` | Nothing resolved yet — wait, or check the manager log for resolve errors. |
| ImageSet | `Ready=False` | `Unbound` | No, or more than one, `MirrorTarget` references this ImageSet. |
| ImageSet | `CatalogReady=True` | `CatalogBuildSucceeded` | Filtered catalog image(s) built and pushed. |
| ImageSet | `CatalogReady=False` | `WaitingForOperatorMirror` | Build deferred until no image of the ImageSet is pending (the message says how many are). Expected while mirroring. |
| ImageSet | `CatalogReady=False` | `CatalogBuildRunning` | A catalog-build Job is running. |
| ImageSet | `CatalogReady=False` | `CatalogBuildFailed` | The Job failed — see [Troubleshooting](troubleshooting.md#catalogready-stays-false). |
| MirrorTarget | `Ready=True` | `DeploymentReady` | Manager Deployment exists and exposure is configured. |
| MirrorTarget | `Ready=False` | `ReconcileError` / `ExposureError` | Creating RBAC, the Deployment or the Route/Ingress failed; message has the error. |
| MirrorTarget | `Cleanup=False` | `CleanupInProgress` | Cleanup Jobs running for the ImageSets in `status.pendingCleanup`. |
| MirrorTarget | `Cleanup=True` | `CleanupComplete` | All cleanups finished. |
| MirrorTarget | `Cleanup=False` | `CleanupError` | Creating a cleanup Job failed. |

`observedGeneration` on every condition tells you whether it reflects the current spec.
`ImageSet.status.observedGeneration` is only advanced when the manager resolved the
spec without any upstream error, and `lastSuccessfulPollTime` is the poll clock.

### Other useful views

```bash
# manager log (resolve decisions, dispatch, drift checks)
kubectl logs deployment/internal-registry-manager -n mirror -f

# worker pods and their logs
kubectl get pods -n mirror -l app=oc-mirror-worker
kubectl logs -n mirror -l app=oc-mirror-worker --tail=100

# catalog-build / cleanup jobs
kubectl get jobs -n mirror

# per-image state (dump the gzip JSON of an ImageSet's state ConfigMap)
kubectl get cm ocp-4-16-operators-images -n mirror -o jsonpath='{.binaryData.images\.json\.gz}' \
  | base64 -d | gunzip | jq 'to_entries[] | select(.value.state != "Mirrored")'
```

## Failed images

A copy that fails is retried immediately, up to 10 times. After that the image is
**permanently failed**: it is listed in `status.failedImageDetails` (first 20 by
destination; `failedImages` has the true count) and only retried by the drift check when
it is still missing from the target, by a [recollect](#recollect) or
[force-resync](#force-resync), or after a spec change touching its entry.

```bash
kubectl get imageset ocp-4-16-operators -n mirror -o json | jq '.status.failedImageDetails'
```

```json
{
  "source":      "registry.redhat.io/openshift4/ose-kube-rbac-proxy@sha256:fde63…",
  "destination": "registry.example.com/mirror/openshift4/ose-kube-rbac-proxy:sha256-fde63…",
  "error":       "failed to copy image: MANIFEST_UNKNOWN: manifest unknown",
  "origin":      "registry.redhat.io/redhat/redhat-operator-index:v4.16 [web-terminal] — web-terminal.v1.11.0"
}
```

`origin` names the spec entry (and, for operators, the bundle) that pulled the image in.

| Error | Usual cause | Fix |
|---|---|---|
| `MANIFEST_UNKNOWN`, `NAME_UNKNOWN` | Image no longer exists upstream (removed bundle, retired image) | Nothing to fix on your side; block the image or narrow the package. Report to the vendor using `origin`. |
| `unauthorized`, `UNAUTHORIZED` on pull | Missing credentials for that source registry | Add the registry to the [auth secret](configuration/credentials.md), then recollect. |
| `unauthorized` on push, `failed to send blob post` | Push permission missing; Quay repository auto-creation not allowed | Grant write/create on the target organization. |
| `context deadline exceeded`, `i/o timeout` | Slow or throttled transfer; 20 min per-image timeout hit | Check bandwidth/proxy; reduce `batchSize`; consider `workerStorage`. |
| `signature check failed` | `requireSignedImages` and no cosign signature at the destination | Sign the image, or disable the requirement. |
| `toomanyrequests` | Source registry rate limit | Lower `concurrency`; authenticate to the source registry. |

After fixing the cause, run a [recollect](#recollect).

## Recollect

```bash
kubectl annotate imageset ocp-4-16-operators -n mirror \
  mirror.openshift.io/recollect=$(date +%s) --overwrite
```

The manager re-resolves every entry of the `ImageSet` **ignoring the resolution cache**
(Cincinnati graph, catalog FBC, Helm charts are fetched again), gives failed images —
including permanently failed ones — a fresh retry cycle, rebuilds the OSUS graph image if
enabled, and asks for a rebuild of the filtered catalog once all images are mirrored.
Already mirrored images are left alone. The annotation is removed once the recollect has
been honoured; the value is irrelevant, it just has to change to trigger again. A
recollect requested while a resolution is already running is not lost — the manager only
removes the value it actually honoured.

A permanently failed image keeps its `permanentlyFailed` marker while it is retried, so
it still counts as failed (not pending) in the status and does not hold back the catalog
build. When the manager honours a recollect it records the
`mirror.openshift.io/recollect-honored` annotation; the ImageSet controller turns each
new value into exactly one catalog rebuild once mirroring has finished.

Use it after fixing credentials or network problems, after an upstream image reappeared,
and after spec edits that the cache does not detect (see
[Configuring ImageSets](configuration/imagesets.md#where-the-catalog-image-goes)).

The console plugin exposes this as **Recollect** on the ImageSet page.

## Force resync

```bash
kubectl annotate imageset ocp-4-16-operators -n mirror \
  mirror.openshift.io/force-resync=$(date +%s) --overwrite
```

Resets **every** image of the `ImageSet` to `Pending`, mirrored or not, so all of them
are copied again (images in flight in a worker at that moment are left to finish).
Registries deduplicate blobs, so the cost is mostly manifest traffic, but for large
`ImageSet`s this still takes a while. Use it after target-registry data loss, a restore
from an older backup, or suspected corruption. Available as **Force Resync** in the
console plugin.

## Changing an ImageSet

Any edit is picked up on the next manager tick (≤ 30 s):

1. Entries whose signature changed are re-resolved; unchanged entries are served from
   the cache.
2. New images are added as `Pending` and dispatched.
3. Images no longer produced by any entry lose their reference. If no other `ImageSet`
   of the target needs them they become orphans (see [Cleanup](#cleanup)).
4. `ImageSet.status.observedGeneration` advances once the new spec resolved cleanly.
5. If operator entries changed, the filtered catalog is rebuilt once nothing is
   pending.

Removing an `ImageSet` from `MirrorTarget.spec.imageSets` stops mirroring it; its state
ConfigMap is handed to the cleanup path.

## Cleanup

Cleanup is opt-in per `MirrorTarget`:

```bash
kubectl annotate mirrortarget internal-registry -n mirror \
  mirror.openshift.io/cleanup-policy=Delete
```

| Trigger | What is deleted |
|---|---|
| `ImageSet` removed from `spec.imageSets` | Every image that only this `ImageSet` referenced. Images shared with a remaining `ImageSet` stay. |
| Spec narrowed or image blocked | The images that dropped out of every `ImageSet` of the target (collected in `<target>-images-orphans`). |

Each cleanup runs as a Job named `cleanup-<target>-<imageset>-<hash>` (or `…-orphans-…`)
that deletes the manifests from the registry. Progress is visible in
`MirrorTarget.status.pendingCleanup` and the `Cleanup` condition; a failed Job is
recreated on the next reconcile until it succeeds.

Things to know before enabling it:

- **`kubectl delete imageset` triggers no cleanup.** Remove the name from the
  `MirrorTarget` first, wait for `pendingCleanup` to empty, then delete the object.
- Tag references are removed as **tags** (OCI tag delete, or re-pointing the tag before
  deleting), so component images of a remaining release version that share a digest in
  `openshift/release` are not affected. Digest references delete the manifest.
- Before a cleanup Job is created, its snapshot is filtered against the live state of
  every ImageSet of the target: an image that was orphaned earlier but is needed again
  (package re-added, version range widened) is never deleted.
- Registry garbage collection is the registry's job. Quay and Distribution keep blobs
  until their own GC runs.
- Enabling the policy later only affects orphans produced from then on; without the
  policy the orphans ConfigMap is cleared instead of accumulating.

To remove content **without** deleting it from the registry, simply do not set the
annotation.

## Restarting components

- **Manager:** `kubectl rollout restart deployment/<target>-manager -n mirror`. State is
  reloaded from the ConfigMaps; running workers are re-adopted from their pod
  annotations. A restart also triggers an immediate drift check.
- **Workers:** deleting a worker pod returns its images to `Pending`; they are
  dispatched again on the next tick.
- **Controller:** safe to restart at any time; it holds no state.

## Monitoring

The controller and every manager expose Prometheus metrics; on clusters with the
prometheus-operator CRDs the operator creates `ServiceMonitor`s, a `PrometheusRule` and
(on OpenShift) a console dashboard automatically. Metric names, alert definitions and
setup notes are in the [metrics reference](reference/metrics.md). The most useful signals:

| Metric | Use |
|---|---|
| `oc_mirror_mirrortarget_images_pending` / `_failed` | Progress and health per target |
| `oc_mirror_manager_images_mirrored_total` | Throughput; flat while pending > 0 means stuck |
| `oc_mirror_imageset_last_poll_seconds` | Detect ImageSets whose polling stopped |
| `oc_mirror_reconcile_errors_total` | Controller-side errors |

Built-in alerts: `OCMirrorHighFailedImages`, `OCMirrorAllImagesFailed`,
`OCMirrorReconcileErrors`, `OCMirrorNoProgress`, `OCMirrorManagerDown`.

## Known issues

Behaviour that differs from what this documentation describes is tracked in the
[issue tracker](https://github.com/mariusbertram/oc-mirror-operator/issues?q=is%3Aissue+is%3Aopen+label%3Abug).
Planned improvements (retry backoff, readiness probe, callback latency under load)
carry the `enhancement` label.
