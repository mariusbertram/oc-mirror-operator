# Metrics and alerts

## Endpoints

| Component | Endpoint | Scraped by |
|---|---|---|
| Controller | metrics port of the controller Deployment (`:8443`, HTTPS, via `--metrics-bind-address`) | `ServiceMonitor` `oc-mirror-controller` |
| Manager | `:9090/metrics` (plain HTTP; also `/healthz`) | `ServiceMonitor` `oc-mirror-manager` (selects `app=oc-mirror-manager`) |

The `MonitoringReconciler` creates both ServiceMonitors, the PrometheusRule below and
the dashboard ConfigMap `oc-mirror-dashboard` in `openshift-config-managed` (visible
under **Observe → Dashboards** in the OpenShift console). ServiceMonitor/PrometheusRule
are only created when the prometheus-operator CRDs exist. On OpenShift, enable
user-workload monitoring so the ServiceMonitors are picked up.

## Metrics

### Controller (`oc_mirror_…`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `oc_mirror_mirrortarget_images_total` | gauge | `namespace`, `target` | Distinct images across the target's ImageSets |
| `oc_mirror_mirrortarget_images_mirrored` | gauge | `namespace`, `target` | Successfully mirrored |
| `oc_mirror_mirrortarget_images_pending` | gauge | `namespace`, `target` | Pending or in flight |
| `oc_mirror_mirrortarget_images_failed` | gauge | `namespace`, `target` | Permanently failed |
| `oc_mirror_reconcile_errors_total` | counter | `namespace`, `name`, `controller` | Reconcile errors per resource and controller (`mirrortarget`, `imageset`, `mirrorexport`) |
| `oc_mirror_imageset_last_poll_seconds` | gauge | `namespace`, `imageset` | Unix time of the last successful upstream poll |

### Manager (`oc_mirror_manager_…`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `oc_mirror_manager_batches_total` | counter | `target`, `result` (`success`/`failed`) | Worker batches dispatched |
| `oc_mirror_manager_images_mirrored_total` | counter | `target`, `imageset` | Images reported mirrored by workers |
| `oc_mirror_manager_images_failed_total` | counter | `target`, `imageset` | Images reported failed by workers |
| `oc_mirror_manager_batch_duration_seconds` | histogram | `target` | Worker batch duration |
| `oc_mirror_manager_active_workers` | gauge | `target` | Worker pods currently in progress |
| `oc_mirror_manager_worker_retries_total` | counter | `target` | Retry attempts (failure reports and signature failures) |

## Alerts (`PrometheusRule oc-mirror`)

| Alert | Expression | Meaning |
|---|---|---|
| `OCMirrorHighFailedImages` | `oc_mirror_mirrortarget_images_failed > 10` | More than ten permanently failed images on a target |
| `OCMirrorAllImagesFailed` | failed / total `> 0.5` | Most of a target's images are failing — credentials or connectivity |
| `OCMirrorReconcileErrors` | `rate(oc_mirror_reconcile_errors_total[5m]) > 0` | Controller reconciles are erroring |
| `OCMirrorNoProgress` | pending `> 0` and no mirrored images in 30 min | Mirroring is stuck |
| `OCMirrorManagerDown` | `absent(oc_mirror_manager_active_workers)` | No manager metrics at all — manager pods down or not scraped |

## Useful queries

```promql
# progress per target
oc_mirror_mirrortarget_images_mirrored / oc_mirror_mirrortarget_images_total

# hours since the last successful poll
(time() - oc_mirror_imageset_last_poll_seconds) / 3600

# images per minute
rate(oc_mirror_manager_images_mirrored_total[10m]) * 60
```
