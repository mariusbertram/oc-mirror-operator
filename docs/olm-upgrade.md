# OLM Upgrade Guide

This guide describes how to upgrade the `oc-mirror-operator` from one version
to the next using the Operator Lifecycle Manager (OLM).

---

## Table of Contents

1. [Overview](#overview)
2. [Upgrade Path](#upgrade-path)
3. [Pre-Upgrade Checklist](#pre-upgrade-checklist)
4. [Performing the Upgrade](#performing-the-upgrade)
5. [Post-Upgrade Steps](#post-upgrade-steps)
6. [Migration Notes by Version](#migration-notes-by-version)
7. [Rolling Back](#rolling-back)
8. [Troubleshooting](#troubleshooting)

---

## Overview

`oc-mirror-operator` follows a continuous upgrade strategy: each release is an
incremental update in an OLM channel. The upgrade is performed by updating the
CatalogSource that serves the operator bundle and then triggering an InstallPlan.

The operator uses `replaces:` in its CSV to declare a linear upgrade graph:

```
v0.0.1 → v0.0.2 → v0.0.3 → v0.0.4 → v0.0.5 → v0.0.6
```

OLM will apply each intermediate step automatically when upgrading across
multiple versions.

---

## Upgrade Path

| From | To | Notes |
|---|---|---|
| v0.0.1 | v0.0.2 | Adds proxy, CA bundle, worker PVC; operator catalog filtering. |
| v0.0.2 | v0.0.3 | Bundle CSV updated with correct PVC and RBAC permissions. |
| v0.0.3 | v0.0.4 | Operator icon restored; OLM upgrade e2e test added. |
| v0.0.4 | v0.0.5 | Release workflow fix only — no functional change. |
| v0.0.5 | v0.0.6 | RBAC resource names change to per-MirrorTarget; proxy FQDN fix; tag@digest support. See [Migration Notes](#migration-notes-by-version). |

---

## Pre-Upgrade Checklist

- [ ] Verify the current operator version: `kubectl get csv -n oc-mirror-operator`
- [ ] Check that all MirrorTargets are in a stable state (no ongoing mirroring):
  ```bash
  kubectl get mirrortargets -n oc-mirror-operator
  ```
- [ ] Back up existing MirrorTarget and ImageSet CRs:
  ```bash
  kubectl get mirrortargets,imagesets -n oc-mirror-operator -o yaml > backup.yaml
  ```
- [ ] Read the [Migration Notes](#migration-notes-by-version) for any breaking
  changes in the target version.

---

## Performing the Upgrade

### Option A: Update via CatalogSource (recommended)

If you installed the operator from a CatalogSource pointing to a bundle index
image, update the index image tag to include the new bundle:

```bash
kubectl patch catalogsource oc-mirror-operator-catalog \
  -n olm \
  --type merge \
  -p '{"spec":{"image":"ghcr.io/mariusbertram/oc-mirror-operator-bundle:latest"}}'
```

OLM will detect the new CSV in the catalog and create a new InstallPlan.

### Option B: Manual InstallPlan approval

If the Subscription's `installPlanApproval` is `Manual`:

```bash
# List pending InstallPlans
kubectl get installplans -n oc-mirror-operator

# Approve the InstallPlan
kubectl patch installplan <name> \
  -n oc-mirror-operator \
  --type merge \
  -p '{"spec":{"approved":true}}'
```

### Option C: Automatic approval

If `installPlanApproval: Automatic`, OLM applies the upgrade automatically once
the new bundle appears in the catalog. Monitor progress with:

```bash
kubectl get csv -n oc-mirror-operator -w
```

---

## Post-Upgrade Steps

1. **Verify the new CSV is in `Succeeded` phase**:
   ```bash
   kubectl get csv -n oc-mirror-operator
   ```
2. **Check operator pod is running**:
   ```bash
   kubectl get pods -n oc-mirror-operator
   ```
3. **Verify MirrorTargets are reconciling normally**:
   ```bash
   kubectl get mirrortargets -n oc-mirror-operator
   kubectl describe mirrortarget <name> -n oc-mirror-operator
   ```
4. **Apply any version-specific migration steps** listed below.

---

## Migration Notes by Version

### v0.0.6 — Per-MirrorTarget RBAC names

**Breaking change**: The operator now creates RBAC resources with per-MirrorTarget
names (`{mt.Name}-coordinator`, `{mt.Name}-worker`) instead of the fixed names
`oc-mirror-coordinator` and `oc-mirror-worker`.

After upgrading to v0.0.6, old fixed-name resources are orphaned (they have no
owner reference that triggers garbage collection). **Delete them manually**:

```bash
kubectl delete serviceaccount oc-mirror-coordinator oc-mirror-worker \
  -n oc-mirror-operator

kubectl delete role oc-mirror-coordinator oc-mirror-worker \
  -n oc-mirror-operator

kubectl delete rolebinding oc-mirror-coordinator oc-mirror-worker \
  -n oc-mirror-operator
```

> This only needs to be done once per namespace where the operator was deployed
> prior to v0.0.6.

### v0.0.6 — Proxy KUBERNETES_SERVICE_HOST override

If you have a proxy configured (`spec.proxy.httpsProxy`), the operator now
automatically overrides `KUBERNETES_SERVICE_HOST` to
`kubernetes.default.svc.cluster.local` in all pod specs. This fixes connectivity
between pods and the Kubernetes API in proxy environments. No action required —
the fix is applied automatically when pods are restarted after upgrade.

### v0.0.2 — New required RBAC for PVC support

v0.0.2 adds the ability to provision ephemeral PVCs for worker pods
(`spec.workerStorage`). The bundle CSV was updated in v0.0.3 to include the
required `persistentvolumeclaims` RBAC permissions. If you skipped v0.0.3 and
are upgrading directly from v0.0.1 to v0.0.4+, OLM will apply the full
upgrade graph and include these permissions automatically.

---

## Rolling Back

OLM does not support automatic rollback. To roll back manually:

1. Scale down the operator:
   ```bash
   kubectl scale deployment oc-mirror-operator-controller-manager \
     -n oc-mirror-operator --replicas=0
   ```
2. Restore the previous CSV by pointing the CatalogSource back to the prior
   bundle index.
3. Delete the current CSV to allow OLM to reinstall the previous version:
   ```bash
   kubectl delete csv oc-mirror-operator.v0.0.6 -n oc-mirror-operator
   ```
4. Approve the rollback InstallPlan if approval is `Manual`.

> **Note**: CRD schema changes are not rolled back automatically. If a newer
> version added new CRD fields, those fields remain in the cluster schema after
> rollback but are ignored by the older operator version.

---

## Troubleshooting

### CSV stuck in `Installing` phase

Check the OLM operator pod logs:

```bash
kubectl logs -n olm deploy/olm-operator
```

Common causes:
- Pending InstallPlan waiting for approval.
- Image pull error for the new operator image.
- RBAC conflict from prior version resources (see migration notes).

### Manager pod not starting after upgrade

```bash
kubectl describe pod -n oc-mirror-operator -l app=oc-mirror-operator
```

Check for:
- Missing ServiceAccount (`{mt.Name}-coordinator`) — the operator reconciler
  creates it on the first reconcile after upgrade; wait a few seconds and
  check again.
- Old fixed-name SA still referenced: ensure the orphaned `oc-mirror-coordinator`
  SA is deleted.

### MirrorTarget shows `ManagerNotReady`

The condition clears once the manager pod starts. If it persists, check:

```bash
kubectl get pods -n oc-mirror-operator
kubectl describe mirrortarget <name> -n oc-mirror-operator
```

### Proxy: manager cannot reach the Kubernetes API

Ensure you have upgraded to v0.0.6+, which automatically overrides
`KUBERNETES_SERVICE_HOST` to the FQDN. For older versions, add
`kubernetes.default.svc.cluster.local` to `spec.proxy.noProxy` explicitly:

```yaml
spec:
  proxy:
    noProxy: "kubernetes.default.svc.cluster.local"
```
