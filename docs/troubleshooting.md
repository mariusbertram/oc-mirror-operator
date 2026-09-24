# Troubleshooting

Symptom-oriented. Each entry names where to look, the usual cause and the fix. For the
meaning of conditions see [Operations → Reading status](operations.md#reading-status).

**Contents**

- [First stop: where the logs are](#first-stop-where-the-logs-are)
- [The manager pod does not start](#the-manager-pod-does-not-start)
- [ImageSet stays Ready=False / Empty](#imageset-stays-readyfalse--empty)
- [Images stay Pending](#images-stay-pending)
- [Images fail with registry errors](#images-fail-with-registry-errors)
- [CatalogReady stays False](#catalogready-stays-false)
- [Releases are skipped](#releases-are-skipped)
- [Mirrored images disappear or come back](#mirrored-images-disappear-or-come-back)
- [Resource API / console plugin problems](#resource-api--console-plugin-problems)
- [Proxy problems](#proxy-problems)
- [Quay specifics](#quay-specifics)
- [Collecting information for a bug report](#collecting-information-for-a-bug-report)

---

## First stop: where the logs are

| Component | Command |
|---|---|
| Controller | `kubectl logs deployment/oc-mirror-operator-controller-manager -n oc-mirror-operator` |
| Manager | `kubectl logs deployment/<target>-manager -n <ns> -f` |
| Workers | `kubectl logs -n <ns> -l app=oc-mirror-worker --tail=200` |
| Catalog build | `kubectl logs job/<job> -n <ns>` (`kubectl get jobs -n <ns>`) |
| Cleanup | `kubectl logs job/cleanup-<target>-… -n <ns>` |
| Events | `kubectl get events -n <ns> --sort-by=.lastTimestamp` |

The manager log is the most informative one: it prints every resolve decision
(`cache hit`, `probe catalog`, `Warning: …`), each dispatched batch, and each drift-check
result.

## The manager pod does not start

| Symptom | Cause | Fix |
|---|---|---|
| No `<target>-manager` Deployment | Controller could not reconcile the MirrorTarget | `MirrorTarget` condition `Ready=False/ReconcileError`; controller log. |
| `ImagePullBackOff` | `MANAGER_IMAGE` in the controller Deployment points at an unreachable image | Check the CSV/Deployment env; in disconnected setups the operator images themselves must be mirrored first. |
| `CreateContainerConfigError`, `secret … not found` | `spec.authSecret` does not exist in the namespace, or lacks `.dockerconfigjson` | Create the secret in the right format ([credentials](configuration/credentials.md)). |
| `WORKER_IMAGE environment variable is required` in the manager log | Controller Deployment is missing the env var | Re-apply the deployment / CSV. |
| Manager restarts every ~90 min | Liveness probe found the reconcile loop stalled for > 1.5 h | Look for a hung resolve in the log (huge catalog, unreachable registry with no timeout at the proxy). |

## ImageSet stays Ready=False / Empty

Nothing was resolved. Look in the manager log for `Warning: probe …`, `Warning: collect …`
or `failed to resolve ImageSet`:

| Log line | Cause | Fix |
|---|---|---|
| `probe release channel …: unexpected status code 404` | Channel name does not exist for the architecture | Check the channel name (`stable-4.16`, `fast-4.16`, `eus-4.16`, `candidate-4.16`). |
| `probe release channel …: dial tcp … i/o timeout` | No route to `api.openshift.com` | [Network requirements](reference/network.md); configure `spec.proxy`. |
| `probe catalog …: unauthorized` | No credentials for `registry.redhat.io` | Add them to the auth secret. |
| `no release nodes for channel … passed signature verification` | Signatures could not be fetched from `mirror.openshift.com`, or the payloads are unsigned (OKD, CI) | Allow the host, or set `skipSignatureVerification: true` on the channel. |
| `signature verification failed for catalog …` | `signatureVerification` configured and the catalog is not signed with that key | Fix the key or remove the block. |
| `ImageSet … is referenced by multiple MirrorTargets` | Same ImageSet listed in two targets | Condition `Unbound`; remove one reference. |

## Images stay Pending

```bash
kubectl get pods -n <ns> -l app=oc-mirror-worker
```

| Observation | Cause | Fix |
|---|---|---|
| No worker pods at all | `concurrency` slots are taken by a pod that is stuck | Pods stuck in `Pending` for 15 min are deleted automatically. Check `kubectl describe pod` for scheduling problems (node selector, tolerations, quota, PVC binding for `workerStorage`). |
| Worker pods `ImagePullBackOff` | `WORKER_IMAGE` unreachable | Same as the manager image above. |
| Workers run but nothing turns `Mirrored` | Callbacks to the manager fail | Worker log: `Status callback attempt … failed`. The `<target>-manager-ingress` NetworkPolicy only allows pods labelled `app=oc-mirror-worker,mirrortarget=<target>` on port 8080 — a CNI without NetworkPolicy support is fine, a broken one is not. |
| Workers log `Skipping …: no longer required` | The images were removed from the spec while the batch was in flight | Expected. |
| Everything stalls during a huge drift check | Old versions ran the drift check inline | Upgrade; current versions run it in the background. |

## Images fail with registry errors

See the table in [Operations → Failed images](operations.md#failed-images) for the
mapping from error text to cause. To test credentials outside the operator:

```bash
kubectl run skopeo --rm -it --restart=Never --image=quay.io/skopeo/stable:latest \
  --overrides='{"spec":{"volumes":[{"name":"auth","secret":{"secretName":"registry-creds"}}],
  "containers":[{"name":"skopeo","image":"quay.io/skopeo/stable:latest",
  "command":["skopeo","inspect","--authfile","/auth/.dockerconfigjson","docker://registry.redhat.io/ubi9/ubi:latest"],
  "volumeMounts":[{"name":"auth","mountPath":"/auth"}]}]}}' -n <ns>
```

## CatalogReady stays False

```bash
kubectl get imageset <name> -n <ns> -o json | jq '.status.conditions[] | select(.type=="CatalogReady")'
kubectl get jobs -n <ns>
```

| Reason | Meaning | Fix |
|---|---|---|
| `WaitingForOperatorMirror` with "N pending" | Expected while images are still being copied. The build only starts when every image of the ImageSet is `Mirrored` or permanently failed, so the catalog never advertises a bundle that is not in the registry. | Wait; fix failed images. |
| `WaitingForOperatorMirror` with "waiting for the manager to resolve the current spec" | `observedGeneration` lags `generation` — the last resolve hit an upstream error and will be retried | Manager log. |
| `WaitingForOperatorMirror` with "no resolved catalog digest recorded" | State written by an older version | Trigger a recollect once. |
| `CatalogBuildFailed` | The Job failed 3 times | `kubectl logs job/<job>`; typical causes are push permission on the target, or an `opm`-side validation error (multiple channel heads) — please report those with the log. |
| `CatalogBuildRunning` for > 30 min | Job hit its 30 min deadline | Slow registry; the Job is retried. |

## Releases are skipped

The manager log says `skipping until signed` or `has no digest`. Release payloads must
have a GPG signature on `mirror.openshift.com` signed by the Red Hat release keys.
Unpublished, nightly, CI and OKD payloads do not; set `skipSignatureVerification: true`
on that channel.

## Mirrored images disappear or come back

- **Images deleted from the registry get re-mirrored** — that is the drift check doing
  its job (every `checkExistInterval`). If you removed them on purpose, remove them from
  the spec.
- **Images vanish after a routine poll and come back later** — a catalog's heads-only
  selection advanced to a newer bundle; the old bundle became an orphan. With
  `cleanup-policy: Delete` it was deleted. This is expected for heads-only; use
  `previousVersions` or explicit channels to keep older bundles.

## Resource API / console plugin problems

| Symptom | Fix |
|---|---|
| `curl` to the Route returns 404 for every path | Check the path: `/api/v1/targets/<target>/imagesets/<imageset>/idms.yaml`. The Service is `<target>-resources`, backed by the `oc-mirror-resource-api` Deployment on 8081. |
| Route does not exist on OpenShift | `MirrorTarget` condition `ExposureError`; the controller needs `routes` and `routes/custom-host` permissions (part of the CSV). |
| Console plugin tab missing | Only deployed when the `ConsolePlugin` CRD exists and `PLUGIN_IMAGE` is set. Check `kubectl get consoleplugin oc-mirror-operator` and the `oc-mirror-plugin` Deployment in the operator namespace. The console operator can take a minute to load a new plugin. |
| Edits in the plugin fail with 403 | The plugin forwards *your* console token; you need `patch` on `imagesets`/`mirrortargets` in that namespace. |
| Release channel drop-down is empty in an air-gapped cluster | The plugin backend queries `api.github.com` for the channel list; provide the `oc-mirror-ocp-versions` ConfigMap described in [Network requirements](reference/network.md#air-gapped-channel-discovery). |

## Proxy problems

| Symptom | Cause | Fix |
|---|---|---|
| Manager cannot reach the Kubernetes API | Proxy intercepts the ClusterIP | Fixed automatically when `spec.proxy` is set (`KUBERNETES_SERVICE_HOST` is rewritten to the FQDN, which is in `NO_PROXY`). If it still fails, the *controller* pod lacks proxy settings — configure them via the Subscription/Deployment. |
| Workers cannot reach the manager | `.svc.cluster.local` not excluded | Auto-injected; verify with `kubectl get deploy <target>-manager -o jsonpath='{.spec.template.spec.containers[0].env}'`. |
| TLS errors behind an intercepting proxy | Proxy re-signs TLS with a private CA | Add the CA via `spec.caBundle`. |

## Quay specifics

| Symptom | Cause | Fix |
|---|---|---|
| `failed to send blob post: unauthorized` with correct credentials | The robot account may not create repositories | Grant *Create repositories* / admin on the organization, or pre-create the repos. |
| `BLOB_UPLOAD_UNKNOWN` on large layers | Upload session expired during a slow transfer | Already mitigated by disk buffering; ensure `workerStorage` is large enough for the biggest layer. |
| `digest mismatch` on parallel uploads | Quay storage race on concurrent identical blobs | Use `concurrency: 1`. |
| HTTP 400 during drift checks behind an OpenShift Route | Bearer token grew beyond HAProxy's 8 KB header limit | Self-healing: the manager refreshes its registry client and retries. |

## Collecting information for a bug report

```bash
NS=<ns>; MT=<target>; IS=<imageset>
kubectl get mirrortarget $MT -n $NS -o yaml > mt.yaml
kubectl get imageset $IS -n $NS -o yaml > is.yaml
kubectl logs deployment/$MT-manager -n $NS --tail=2000 > manager.log
kubectl logs deployment/oc-mirror-operator-controller-manager -n oc-mirror-operator --tail=2000 > controller.log
kubectl get events -n $NS --sort-by=.lastTimestamp > events.txt
kubectl get cm $IS-images -n $NS -o jsonpath='{.binaryData.images\.json\.gz}' | base64 -d | gunzip > state.json
```

Redact registry hosts and credentials before attaching them to an
[issue](https://github.com/mariusbertram/oc-mirror-operator/issues).
