# Quick Start Guides

This page provides **focused, scenario-based quick start guides** to get you up and running with oc-mirror-operator for common use cases. Each guide covers a specific mirroring scenario.

**Choose your scenario:**

| Use Case | Guide |
|---|---|
| Mirror a single operator | [Single Operator](quickstart-operator.md) |
| Install the operator itself in a disconnected cluster | [Bootstrap the Bootstrapper](quickstart-bootstrap-operator.md) |
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
kubectl version --client=false | grep Server

# Check if OLM is available (for OLM-based installation)
kubectl get crd catalogsources.operators.coreos.com &>/dev/null && echo "✅ OLM installed" || echo "⚠️  OLM not found"

# Check if you have a registry to push to
echo "Registry endpoint: ${REGISTRY:-not set}"
```

### Common Setup Steps

All guides share the same initial steps — they live in the canonical
[Setup guide](setup.md):

1. **[Install the operator](setup.md#1-install-the-operator)** — OLM
   (OpenShift), static release manifest (plain Kubernetes) or from source
2. **[Prepare the registry credentials](setup.md#2-prepare-the-registry-credentials)** — one
   combined dockerconfigjson secret for source pull and target push access

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
