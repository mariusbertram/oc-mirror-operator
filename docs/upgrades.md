# Upgrading

**Contents**

- [Upgrading with OLM](#upgrading-with-olm)
- [Upgrading a manifest-based installation](#upgrading-a-manifest-based-installation)
- [What to check afterwards](#what-to-check-afterwards)
- [Rolling back](#rolling-back)
- [Version notes](#version-notes)

---

## Upgrading with OLM

Releases are published as OLM bundles in the `brtrm-dev-catalog` catalog; the CSVs form
a linear upgrade graph, so OLM steps through intermediate versions automatically.

1. **Before**: note the current version and make sure nothing is mid-flight that you
   would mind repeating.
   ```bash
   kubectl get csv -n oc-mirror-operator
   kubectl get mirrortargets,imagesets -n oc-mirror-operator
   kubectl get mirrortargets,imagesets,mirrorexports -n oc-mirror-operator -o yaml > backup.yaml
   ```
   In-flight worker batches are simply re-dispatched by the new manager; state is in
   ConfigMaps and survives.
2. **Refresh the catalog** if it is pinned to a tag, otherwise OLM sees the new bundle
   on its own:
   ```bash
   kubectl patch catalogsource brtrm-dev-catalog -n openshift-marketplace --type merge \
     -p '{"spec":{"image":"quay.io/mariusbertram/brtrm-dev-catalog:latest"}}'
   ```
3. **Approve** the InstallPlan if the Subscription uses `installPlanApproval: Manual`:
   ```bash
   kubectl get installplans -n oc-mirror-operator
   kubectl patch installplan <name> -n oc-mirror-operator --type merge -p '{"spec":{"approved":true}}'
   ```
4. Watch `kubectl get csv -n oc-mirror-operator -w` until the new CSV is `Succeeded`.

The new controller rolls out the manager Deployments (new `MANAGER_IMAGE`) on its first
reconcile; the manager reloads state from the ConfigMaps. Catalog-build Jobs are re-run
when the operator image changed, because the image is part of the build signature.

## Upgrading a manifest-based installation

```bash
git fetch && git checkout <new tag>
make install                                                     # CRDs
make deploy IMG=ghcr.io/mariusbertram/oc-mirror-operator-controller:<new tag>
```

`make deploy` sets `MANAGER_IMAGE`, `WORKER_IMAGE` and `PLUGIN_IMAGE` to the matching
tags.

## What to check afterwards

```bash
kubectl get pods -n oc-mirror-operator                 # controller, managers, plugin, resource-api
kubectl get mirrortargets,imagesets -n oc-mirror-operator
kubectl logs deployment/<target>-manager -n oc-mirror-operator | head -50
```

A one-time full re-resolution after an upgrade is normal when the operator's catalog
cache version changed (the log shows `stale cache annotations`); images already
mirrored are not copied again.

## Rolling back

OLM has no automatic rollback. Manually:

1. Scale the controller to 0.
2. Point the CatalogSource at the previous catalog image, delete the current CSV, let
   OLM install the previous one (approve the InstallPlan if needed).
3. Scale back up.

CRD schema additions stay in the cluster; older operators ignore unknown fields.
Downgrading across the v0.0.x → v0.1.0 boundary is not supported (state ConfigMap
layout changed).

## Version notes

Only versions with user-visible changes are listed; see [CHANGELOG.md](../CHANGELOG.md)
for everything else.

### v0.1.0 — modular images

The single `oc-mirror-operator` image with subcommands was split into
`oc-mirror-operator-controller`, `-manager`, `-worker` (+ `-plugin`). OLM handles this
transparently. Manifest-based installs must deploy the new controller Deployment
(`make deploy`), which carries the `MANAGER_IMAGE`/`WORKER_IMAGE`/`PLUGIN_IMAGE`
variables. The old `cmd/main.go` all-in-one binary is deprecated. State ConfigMaps are
migrated automatically from the consolidated `<target>-images` map to one map per
ImageSet plus the shared index on the manager's first start.

### v0.0.11 — TLS fallback order, OCI-layout catalog builder

With `insecure: true` the registry client now tries plain HTTP first and falls back to
HTTPS without verification (previously the other way round, which cost ~60 s per
request against HTTP-only registries). Catalog builds download the source catalog once
into an OCI layout. No configuration changes.

### v0.0.7 — heads-only operator filtering

Packages listed without channels or version ranges now mirror only the newest bundle of
each channel (oc-mirror v2 behaviour) instead of every version. A one-time
re-resolution runs after the upgrade. To keep older bundles use `previousVersions` or
list channels explicitly.

### v0.0.6 — per-MirrorTarget RBAC, proxy fix

ServiceAccounts/Roles/RoleBindings are named `<target>-coordinator` and `<target>-worker`
instead of the fixed `oc-mirror-coordinator`/`oc-mirror-worker`. The old fixed-name
objects are orphaned and must be deleted once:

```bash
kubectl delete sa,role,rolebinding oc-mirror-coordinator oc-mirror-worker -n oc-mirror-operator
```

`KUBERNETES_SERVICE_HOST` is rewritten to the FQDN when `spec.proxy` is set, so pods
behind a proxy reach the API server without extra `noProxy` entries.

### v0.0.2 / v0.0.3 — worker PVCs

`spec.workerStorage` was introduced; v0.0.3 added the `persistentvolumeclaims` RBAC the
feature needs. Skipping straight from v0.0.1 to a newer version is fine — OLM applies
the full graph.
