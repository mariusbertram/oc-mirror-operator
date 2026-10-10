# Quick Start: Mirror a Single Operator  
**Use case:** Mirror a single operator package (e.g., `web-terminal`) from the Red Hat operator catalog to your private registry.

---

## 1. Install the Operator

Follow the canonical [Setup guide](setup.md#1-install-the-operator).
Options: [OLM](setup.md#option-a-olm-recommended-for-openshift),
[static release manifest](setup.md#option-b-static-release-manifest-plain-kubernetes),
[from source](setup.md#option-c-from-source).

---

## 2. Prepare Registry Credentials

Follow [Prepare the Registry Credentials](setup.md#2-prepare-the-registry-credentials)
in the Setup guide. You need pull credentials for the Red Hat catalog
(`registry.redhat.io`) and push credentials for your target registry.

---

## 3. Create the ImageSet

An `ImageSet` defines **what** to mirror. For a single operator:

```yaml
# single-operator-imageset.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: single-operator
  namespace: oc-mirror-operator
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal  # Replace with your desired operator
```

Apply the ImageSet:
```bash
kubectl apply -f single-operator-imageset.yaml
```

**Customize for your use case:**
- Change `web-terminal` to any operator from the [Red Hat catalog](https://catalog.redhat.com/software/operators)
- Change `v4.16` to match your OpenShift version
- For multiple operators, add more entries to the `packages` list

---

## 4. Create the MirrorTarget

A `MirrorTarget` defines **where** to mirror the content:

```yaml
# single-operator-mirrortarget.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-registry
  namespace: oc-mirror-operator
spec:
  registry: registry.example.com/mirror  # Replace with your registry
  authSecret: registry-creds
  imageSets:
    - single-operator
```

Apply the MirrorTarget:
```bash
kubectl apply -f single-operator-mirrortarget.yaml
```

**Customize for your use case:**
- Replace `registry.example.com/mirror` with your registry endpoint and path
- For insecure registries, add `insecure: true`

---

## 5. Watch the Mirroring Process

Monitor the progress:

```bash
# Watch the MirrorTarget and ImageSet status
kubectl get mirrortarget,imageset -n oc-mirror-operator -w

# Example output:
# NAME                          TOTAL   MIRRORED   PENDING   FAILED   AGE
# mirrortarget.my-registry      15      15         0         0        2m
# imageset.single-operator      15      15         0         0        2m
```

View the manager logs:
```bash
kubectl logs deployment/my-registry-manager -n oc-mirror-operator -f
```

View worker pod logs:
```bash
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker
kubectl logs <worker-pod-name> -n oc-mirror-operator -f
```

---

## 6. Verify the Mirror

Once mirroring is complete (`PENDING` and `FAILED` counts are 0), verify the images in your registry:

```bash
# Using skopeo (recommended)
skopeo list-tags docker://registry.example.com/mirror

# Using podman
podman search registry.example.com/mirror

# Using curl (for registries with HTTP API)
curl https://registry.example.com/v2/_catalog
```

Check the operator bundle images are present:
```bash
skopeo list-tags docker://registry.example.com/mirror/redhat/redhat-operator-index
```

---

## 7. Consume the Mirror in a Disconnected Cluster

To use the mirrored operator in a disconnected cluster:

### Get the CatalogSource

```bash
# Using the Resource API (if exposed via Route)
URL=https://$(kubectl get route my-registry-resources -n oc-mirror-operator -o jsonpath='{.spec.host}')
curl -sk $URL/api/v1/targets/my-registry/imagesets/single-operator/catalogsource.yaml

# Or directly from the ConfigMap
kubectl get cm oc-mirror-my-registry-resources -n oc-mirror-operator -o jsonpath='{.data.catalogsource-redhat-operator-index-v4.16\.yaml}'
```

### Apply to Disconnected Cluster

```bash
# 1. Create the namespace
kubectl create namespace openshift-marketplace

# 2. Apply the CatalogSource
kubectl apply -f catalogsource.yaml

# 3. Disable default Red Hat catalogs (on OpenShift)
kubectl patch operatorhub cluster --type merge -p '{"spec":{"disableAllDefaultSources":true}}'

# 4. Create OperatorGroup and Subscription
cat <<EOT | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: my-operators
  namespace: my-namespace
spec:
  targetNamespaces: [my-namespace]
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: web-terminal
  namespace: my-namespace
spec:
  name: web-terminal
  channel: stable
  source: my-registry-redhat-operator-index-v4.16
  sourceNamespace: openshift-marketplace
EOT
```

---

## 8. Clean Up (Optional)

To remove the mirror:

```bash
# Remove the MirrorTarget (this stops mirroring)
kubectl delete mirrortarget my-registry -n oc-mirror-operator

# Remove the ImageSet
kubectl delete imageset single-operator -n oc-mirror-operator

# To also delete the mirrored images from the registry, add the cleanup policy:
kubectl annotate mirrortarget my-registry -n oc-mirror-operator \
  mirror.openshift.io/cleanup-policy=Delete
```

---

## Troubleshooting

| Issue | Solution |
|---|---|
| **Images stay in Pending state** | Check worker pods: `kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker`. If none exist, check manager logs. |
| **Authentication errors** | Verify your `registry-creds` secret contains valid credentials for both source and target registries. |
| **CatalogReady stays False** | The catalog build waits until all images are mirrored. Check for failed images with: `kubectl get imageset single-operator -n oc-mirror-operator -o jsonpath='{.status.failedImageDetails}'` |
| **Registry connection errors** | Verify your registry is accessible and the credentials are correct. For insecure registries, add `insecure: true` to the MirrorTarget spec. |
| **Operator not appearing in catalog** | Ensure the CatalogSource was applied to the disconnected cluster and the default catalogs are disabled. |

For more troubleshooting, see [Troubleshooting](troubleshooting.md).

---

## Next Steps

- [Mirror OpenShift releases](quickstart-release.md) - Add release mirroring
- [Mirror Helm charts](quickstart-helm.md) - Add Helm chart mirroring
- [Full disconnected setup](quickstart-disconnected.md) - Complete air-gapped setup
- [Configuration reference](configuration/imagesets.md) - Explore advanced configuration options

---

## See Also

- [ImageSet Configuration](configuration/imagesets.md#operator-catalogs)
- [MirrorTarget Configuration](configuration/mirrortarget.md)
- [Consuming Results](consuming-results.md)
- [Operations Guide](operations.md)
