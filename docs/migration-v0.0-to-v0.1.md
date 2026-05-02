# Migration Guide: v0.0.x → v0.1.0+

This guide walks you through upgrading from the v0.0.x single-binary architecture to v0.1.0's modular 3-component architecture.

## What Changed?

The operator has been refactored from a single container image with internal subcommands to a modular architecture with three separate container images:

| Component | v0.0.x | v0.1.0+ |
|-----------|--------|---------|
| **Operator Controller** | `oc-mirror-operator:v0.0.x` (subcommand: manager) | `oc-mirror-controller:v0.1.0+` (separate image) |
| **Manager Pod** | `oc-mirror-operator:v0.0.x` (subcommand: manager) | `oc-mirror-manager:v0.1.0+` (separate image) |
| **Worker Pod** | `oc-mirror-operator:v0.0.x` (subcommand: worker) | `oc-mirror-worker:v0.1.0+` (separate image) |
| **Cleanup Job** | `oc-mirror-operator:v0.0.x` (subcommand: cleanup) | `oc-mirror-worker:v0.1.0+` (cleanup subcommand) |

## Benefits

- **Smaller images**: Each component is optimized independently, reducing download/startup time
- **Independent scaling**: Deploy multiple manager instances or worker replicas as needed
- **Better isolation**: Separate RBAC roles per component reduce blast radius
- **Improved maintainability**: Cleaner separation of concerns in the codebase
- **Faster deployments**: Modular architecture enables quicker rollouts and debugging

## Deployment Changes

### v0.0.x: Single-Binary Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oc-mirror-operator
  namespace: oc-mirror-operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app: oc-mirror-operator
  template:
    metadata:
      labels:
        app: oc-mirror-operator
    spec:
      serviceAccountName: oc-mirror-operator
      containers:
      - name: operator
        image: oc-mirror-operator:v0.0.11        # ← Single image
        command: ["/manager"]                     # ← Subcommand
        ports:
        - containerPort: 8080
          name: webhook
```

### v0.1.0+: Modular 3-Component Deployment

```yaml
# 1. Operator Controller Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oc-mirror-controller
  namespace: oc-mirror-operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app: oc-mirror-controller
  template:
    metadata:
      labels:
        app: oc-mirror-controller
    spec:
      serviceAccountName: oc-mirror-controller
      containers:
      - name: controller
        image: oc-mirror-controller:v0.1.0       # ← New image
        command: ["/controller"]                 # ← Dedicated binary

---

# 2. Manager Pod (created per MirrorTarget)
# (Automatically created by the controller — no manual deployment needed)

---

# 3. Worker Pods (created per batch)
# (Automatically created by the manager — no manual deployment needed)
```

## Upgrade Path

### Step 1: Backup Your Configuration

```bash
# Backup existing MirrorTargets and ImageSets
kubectl get mirrortarget -A -o yaml > mirrortargets-backup.yaml
kubectl get imageset -A -o yaml > imagesets-backup.yaml

# Backup ConfigMaps that store image state
kubectl get cm -l mirror.openshift.io/imageset -A -o yaml > imagestate-backup.yaml
```

### Step 2: Uninstall v0.0.x

**Via OLM:**

```bash
kubectl delete subscription oc-mirror-operator -n oc-mirror-operator
kubectl delete csv -n oc-mirror-operator -l operators.coreos.com/oc-mirror-operator.oc-mirror-operator
```

**Manual deployment:**

```bash
kubectl delete deployment oc-mirror-operator -n oc-mirror-operator
kubectl delete svc oc-mirror-operator -n oc-mirror-operator
kubectl delete cm -l mirror.openshift.io/ -n oc-mirror-operator  # Optional: preserve state
```

### Step 3: Install v0.1.0+

**Via OLM (recommended):**

Update your CatalogSource to v0.1.0+ and re-subscribe:

```bash
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

**Manual deployment:**

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Install RBAC (new per-component roles)
kubectl apply -f config/rbac/

# Deploy controller (replaces old manager)
kubectl apply -f config/manager/controller.yaml
```

### Step 4: Verify Installation

```bash
# Check controller deployment
kubectl get deployment -n oc-mirror-operator oc-mirror-controller
# Expected: 1 replica running

# Check for manager pods (auto-created per MirrorTarget)
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-manager

# Check for worker pods (should be empty at this point)
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker
```

### Step 5: Re-Apply Your MirrorTargets and ImageSets

```bash
# Restore configuration
kubectl apply -f mirrortargets-backup.yaml
kubectl apply -f imagesets-backup.yaml

# Wait for manager pods to be created
kubectl wait --for=condition=Ready pod -l app=oc-mirror-manager -n oc-mirror-operator --timeout=60s

# Verify ImageSet status shows mirroring progress
kubectl get imageset -n oc-mirror-operator -o wide
```

## Breaking Changes

### Image Registry Changes

All references to `oc-mirror-operator:vX` must be updated to the new per-component images:

```diff
# Old (deprecated)
- image: oc-mirror-operator:v0.0.11
  command: ["/manager"]

# New
+ image: oc-mirror-controller:v0.1.0
  command: ["/controller"]
```

### RBAC Changes

Service accounts and roles are now per-component. Update any custom RBAC to reference:
- `oc-mirror-controller` (operator controller)
- `{mirrortarget-name}-coordinator` (manager pod)
- `{mirrortarget-name}-worker` (worker pods)

**Example:**

```yaml
# v0.0.x (deprecated)
serviceAccountName: oc-mirror-operator

# v0.1.0+ (new)
serviceAccountName: oc-mirror-controller  # For controller
serviceAccountName: mirrortarget-name-coordinator  # For manager
serviceAccountName: mirrortarget-name-worker      # For worker
```

### Helm Chart Changes

If using Helm charts:

```bash
# Old (single image)
helm install oc-mirror ./helm/oc-mirror \
  --set image=oc-mirror-operator:v0.0.11

# New (per-component images)
helm install oc-mirror ./helm/oc-mirror \
  --set controllerImage=oc-mirror-controller:v0.1.0 \
  --set managerImage=oc-mirror-manager:v0.1.0 \
  --set workerImage=oc-mirror-worker:v0.1.0
```

## Rollback Path

If you need to rollback to v0.0.x after upgrading:

### Step 1: Uninstall v0.1.0+

```bash
# OLM
kubectl delete subscription oc-mirror-operator -n oc-mirror-operator

# Manual
kubectl delete deployment oc-mirror-controller -n oc-mirror-operator
```

### Step 2: Restore v0.0.x

```bash
# OLM
kubectl apply -f subscription-v0.0.11.yaml

# Manual
kubectl apply -f deployment-v0.0.11.yaml
```

### Step 3: Reapply Configuration

```bash
kubectl apply -f mirrortargets-backup.yaml
kubectl apply -f imagesets-backup.yaml
```

## Troubleshooting

### Manager Pod Not Starting

Check if the correct manager image is available:

```bash
kubectl describe deployment oc-mirror-manager -n oc-mirror-operator
kubectl logs deployment/oc-mirror-controller -n oc-mirror-operator
```

### ImageSet Stuck in "Pending"

Verify that worker pods are being created:

```bash
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker

# Check worker pod logs
kubectl logs -l app=oc-mirror-worker -n oc-mirror-operator -f
```

### Quota Exceeded

The modular architecture creates more pods (workers are now separate from manager). Ensure your cluster has sufficient quota:

```bash
kubectl describe resourcequota -n oc-mirror-operator
```

## Deprecated Code Paths

The old `/cmd/main.go` file with subcommands (`manager`, `worker`, `cleanup`) is **deprecated** and will be removed in a future release. It is **no longer used** in v0.1.0+.

New deployments should use:
- `cmd/controller/main.go` – Operator controller
- `cmd/manager/main.go` – Manager pod
- `cmd/worker/main.go` – Worker pods

## Support

If you encounter issues during migration:

1. Check [Troubleshooting](#troubleshooting) section above
2. Review the [Architecture documentation](./user-guide.md#3-concepts)
3. Open an issue on GitHub with your error logs

---

**Migration completed successfully?** Please validate by:
- [ ] Controller deployment running
- [ ] Manager pods created per MirrorTarget
- [ ] Worker pods successfully mirroring images
- [ ] ImageSet status showing `Ready` condition
- [ ] IDMS/ITMS ConfigMaps accessible via Resource API
