# Quick Start Guides

This page provides **focused, scenario-based quick start guides** to get you up and running with oc-mirror-operator for common use cases. Each guide covers a specific mirroring scenario.

**Choose your scenario:**

| Use Case | Guide |
|---|---|
| Mirror a single operator | [Single Operator](quickstart-operator.md) |
| Mirror OpenShift releases | [OpenShift Releases](quickstart-release.md) |
| Mirror Helm charts | [Helm Charts](quickstart-helm.md) |
| Full disconnected cluster setup | [Disconnected Cluster](quickstart-disconnected.md) |

---

## Before You Begin

All quick start guides assume:

- You have a **Kubernetes ≥ 1.26 or OpenShift ≥ 4.12** cluster
- You have **cluster admin privileges** for the target namespace
- You have **write access** to a container registry (Quay, Harbor, registry:2, etc.)
- You have **pull access** to source registries (registry.redhat.io, quay.io, etc.)

### Prerequisites Check

Run this quick check before starting any guide:

```bash
# Check Kubernetes version
kubectl version --short | grep Server

# Check if OLM is available (for OLM-based installation)
kubectl get crd catalogsources.operators.coreos.com &>/dev/null && echo "✅ OLM installed" || echo "⚠️  OLM not found"

# Check if you have a registry to push to
echo "Registry endpoint: ${REGISTRY:-not set}"
```

### Common Setup Steps

All guides share these initial steps:

1. **Install the operator** (choose one method):
   - [OLM (Recommended)](getting-started.md#option-a-olm-recommended) - For OpenShift users
   - [Plain manifests](getting-started.md#option-b-plain-manifests) - For Kubernetes users

2. **Create namespace:**
   ```bash
   kubectl create namespace oc-mirror-operator
   ```

3. **Prepare registry credentials:**
   ```bash
   # For Docker/Podman users
   kubectl create secret generic registry-creds \
     --from-file=.dockerconfigjson=${HOME}/.docker/config.json \
     --type=kubernetes.io/dockerconfigjson -n oc-mirror-operator
   
   # For OpenShift pull secret users
   kubectl get secret/pull-secret -n openshift-config --export -o yaml | \
     kubectl apply -n oc-mirror-operator -f -
   ```

---

## Next Steps

After completing a quick start guide:

1. **Verify your mirror:**
   ```bash
   kubectl get mirrortarget,imageset -n oc-mirror-operator
   ```

2. **Monitor progress:**
   ```bash
   kubectl get pods -n oc-mirror-operator -w
   ```

3. **Consume the mirror:**
   - [Consuming the mirror](consuming-results.md) - For disconnected cluster setup
   - [Operations](operations.md) - For day-2 operations

---

## Troubleshooting Quick Start Issues

| Issue | Solution |
|---|---|
| `ImagePullBackOff` for operator pods | Make sure your cluster can pull the operator images from `ghcr.io/mariusbertram` |
| `CrashLoopBackOff` for manager pod | Check logs: `kubectl logs deployment/<target>-manager -n oc-mirror-operator` |
| `Pending` images not progressing | Check worker pods: `kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker` |
| Authentication errors | Verify your `registry-creds` secret contains credentials for both source and target registries |

For more detailed troubleshooting, see [Troubleshooting](troubleshooting.md).

---

## See Also

- [Getting Started](getting-started.md) - Complete end-to-end guide
- [Concepts](concepts.md) - Understand how the operator works
- [Configuration Reference](configuration/imagesets.md) - Detailed configuration options
