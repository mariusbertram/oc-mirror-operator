# Quick Start: Mirror OpenShift Releases  
**Use case:** Mirror OpenShift release images (including all component images) from the official channels to your private registry.

---

## 1. Install the Operator

Follow the canonical [Setup guide](setup.md#1-install-the-operator).

Choose either:
- [OLM (Recommended for OpenShift)](quickstart-operator.md#option-a-olm-recommended-for-openshift)
- [Plain Manifests (Recommended for Kubernetes)](quickstart-operator.md#option-b-plain-manifests-recommended-for-kubernetes)

---

## 2. Prepare Registry Credentials

Follow [Prepare the Registry Credentials](setup.md#2-prepare-the-registry-credentials) in the Setup guide.

**Important:** For OpenShift releases, your credentials must have access to:
- `registry.redhat.io` (for pulling release images)
- Your target registry (for pushing mirrored images)

---

## 3. Create the ImageSet for OpenShift Releases

An `ImageSet` for OpenShift releases specifies which **channel** and **version range** to mirror:

```yaml
# ocp-releases-imageset.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-16-releases
  namespace: oc-mirror-operator
spec:
  mirror:
    platform:
      architectures: [amd64]  # Can also include: arm64, s390x, ppc64le, multi
      channels:
        - name: stable-4.16
          minVersion: "4.16.0"    # Start from this version
          maxVersion: "4.16.30"   # Up to this version (optional)
          # shortestPath: true     # Uncomment to only mirror upgrade path versions
```

Apply the ImageSet:
```bash
kubectl apply -f ocp-releases-imageset.yaml
```

**Customize for your use case:**

| Option | Description | Example |
|---|---|---|
| **Single version** | Mirror only one specific version | `minVersion: "4.16.20"`, `maxVersion: "4.16.20"` |
| **Latest only** | Mirror only the newest version | `minVersion: "4.16.0"` (no maxVersion) |
| **Version range** | Mirror all versions in a range | `minVersion: "4.16.10"`, `maxVersion: "4.16.25"` |
| **Shortest path** | Only mirror versions on the upgrade path | `shortestPath: true` with min/maxVersion |
| **Multiple architectures** | Mirror for multiple architectures | `architectures: [amd64, arm64]` |
| **OKD releases** | Mirror OKD (OpenShift Kubernetes Distribution) | Add `type: okd` and `skipSignatureVerification: true` |

Example with multiple architectures:
```yaml
spec:
  mirror:
    platform:
      architectures: [amd64, arm64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"
```

Example for OKD:
```yaml
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          type: okd
          skipSignatureVerification: true
```

---

## 4. Include KubeVirt Container Disks (Optional)

To mirror the KubeVirt container disk images (required for OpenShift Virtualization):

```yaml
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"
      kubeVirtContainer: true  # Add this line
```

---

## 5. Include OSUS Graph Data (Optional)

To mirror the OpenShift Update Service (OSUS) graph data image:

```yaml
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"
      graph: true  # Add this line
```

---

## 6. Create the MirrorTarget

```yaml
# ocp-releases-mirrortarget.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-registry
  namespace: oc-mirror-operator
spec:
  registry: registry.example.com/ocp-mirror  # Your registry path
  authSecret: registry-creds
  imageSets:
    - ocp-4-16-releases
  # Optional: Increase concurrency for faster mirroring (default: 1)
  # For non-Quay registries, you can increase this
  # concurrency: 3
  # batchSize: 50
```

Apply the MirrorTarget:
```bash
kubectl apply -f ocp-releases-mirrortarget.yaml
```

**Important for Quay registries:** Keep `concurrency: 1` (the default) to avoid Quay storage backend issues with concurrent uploads of the same blob.

For other registries (Harbor, Nexus, etc.), you can increase concurrency to 3-5 for faster mirroring.

---

## 7. Watch the Mirroring Process

OpenShift releases contain **~190 component images per architecture**, so mirroring takes longer than a single operator:

```bash
# Watch progress
kubectl get mirrortarget,imageset -n oc-mirror-operator -w

# Example output for a release:
# NAME                          TOTAL   MIRRORED   PENDING   FAILED   AGE
# mirrortarget.my-registry      192     45         147       0        5m
# imageset.ocp-4-16-releases      192     45         147       0        5m
```

View detailed logs:
```bash
# Manager logs (shows resolution and dispatch)
kubectl logs deployment/my-registry-manager -n oc-mirror-operator -f

# Worker logs (shows actual image copying)
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker
kubectl logs <worker-pod-name> -n oc-mirror-operator -f
```

---

## 8. Verify the Mirror

Once mirroring is complete, verify the release images in your registry:

```bash
# List all mirrored images
skopeo list-tags docker://registry.example.com/ocp-mirror/openshift/release-images

# Check a specific release
skopeo inspect --raw docker://registry.example.com/ocp-mirror/openshift/release-images:4.16.20-x86_64

# List component images
skopeo list-tags docker://registry.example.com/ocp-mirror/openshift/release
```

---

## 9. Get Release Signatures

OpenShift requires **release signatures** for cluster upgrades. The operator generates these automatically:

```bash
# Get the signatures ConfigMap
kubectl get cm my-registry-signatures -n oc-mirror-operator -o yaml

# Or via the Resource API
URL=https://$(kubectl get route my-registry-resources -n oc-mirror-operator -o jsonpath='{.spec.host}')
curl -sk $URL/api/v1/targets/my-registry/signatures.yaml
```

Apply the signatures to your disconnected cluster:
```bash
curl -sk $URL/api/v1/targets/my-registry/signatures.yaml | kubectl apply -f -
```

---

## 10. Get Mirror Rules (IDMS/ITMS)

The operator generates **ImageDigestMirrorSet (IDMS)** and **ImageTagMirrorSet (ITMS)** resources:

```bash
# Get IDMS (for digest-referenced images)
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/idms.yaml

# Get ITMS (for tag-referenced images)
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/itms.yaml

# Apply to disconnected cluster
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/idms.yaml | kubectl apply -f -
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/itms.yaml | kubectl apply -f -
```

---

## 11. Set Up Update Service (Optional)

If you enabled `graph: true`, set up the OpenShift Update Service to use your mirrored graph data:

```yaml
# On the disconnected cluster
apiVersion: v1
kind: ConfigMap
metadata:
  name: update-service-config
  namespace: openshift-update-service
data:
  graph-data-image: registry.example.com/ocp-mirror/openshift/graph-image:latest
```

---

## 12. Consume the Mirror in a Disconnected Cluster

### Apply Mirror Rules

```bash
# Apply IDMS and ITMS
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/idms.yaml | kubectl apply -f -
curl -sk $URL/api/v1/targets/my-registry/imagesets/ocp-4-16-releases/itms.yaml | kubectl apply -f -

# Apply signatures
curl -sk $URL/api/v1/targets/my-registry/signatures.yaml | kubectl apply -f -
```

### Configure Cluster to Use Mirror

For OpenShift 4.12+, use **IDMS/ITMS** (already applied above). For OpenShift 4.10-4.11, you can use **ImageContentSourcePolicy** as a legacy mechanism:

```bash
cat <<EOT | kubectl apply -f -
apiVersion: operator.openshift.io/v1alpha1
kind: ImageContentSourcePolicy
metadata:
  name: mirror-policy
spec:
  repositoryDigestMirrors:
    - mirrors:
        - registry.example.com/ocp-mirror
      source: registry.redhat.io
EOT
```

---

## 13. Upgrade a Cluster Using the Mirror

To upgrade a disconnected cluster using your mirrored releases:

```bash
# Check available updates
oc adm upgrade

# The cluster should now see the mirrored releases
# and be able to upgrade using them
```

---

## Troubleshooting

| Issue | Solution |
|---|---|
| **Release images not appearing** | Check if the release exists in the channel: `curl https://api.openshift.com/api/upgrades_info/v1/graph?channel=stable-4.16` |
| **Signature verification failed** | For OKD or CI releases, add `skipSignatureVerification: true` to the channel spec. |
| **Too many images pending** | OpenShift releases have ~190 images. This is normal. Check worker logs for progress. |
| **Worker pods crashing** | Check worker logs: `kubectl logs <worker-pod> -n oc-mirror-operator`. Common issue: registry credentials. |
| **Manager pod restarting** | Check manager logs: `kubectl logs deployment/my-registry-manager -n oc-mirror-operator`. Common issue: memory limits. |
| **Images disappearing from registry** | This is normal if you have `cleanup-policy: Delete` and removed the ImageSet. To prevent, don't use cleanup policy or keep the ImageSet. |

For more troubleshooting, see:
- [Troubleshooting Guide](troubleshooting.md)
- [Releases are skipped](troubleshooting.md#releases-are-skipped)
- [Images stay Pending](troubleshooting.md#images-stay-pending)

---

## Performance Tips

| Scenario | Recommendation |
|---|---|
| **Quay registry** | Keep `concurrency: 1` (default) to avoid Quay storage issues |
| **Harbor/Nexus registry** | Increase `concurrency: 3-5` for faster mirroring |
| **Large registries** | Increase `batchSize: 100` (default is 50) |
| **Slow network** | Reduce `batchSize: 10-20` to avoid timeouts |
| **Many releases** | Split into multiple ImageSets (e.g., one per minor version) |

Example for Harbor with higher concurrency:
```yaml
spec:
  concurrency: 5
  batchSize: 100
```

---

## Next Steps

- [Mirror a single operator](quickstart-operator.md) - Add operator mirroring
- [Mirror Helm charts](quickstart-helm.md) - Add Helm chart mirroring
- [Full disconnected setup](quickstart-disconnected.md) - Complete air-gapped setup
- [Advanced release configuration](configuration/imagesets.md#openshift-and-okd-releases)

---

## See Also

- [ImageSet Configuration: Platform](configuration/imagesets.md#openshift-and-okd-releases)
- [MirrorTarget Configuration](configuration/mirrortarget.md)
- [Consuming Results](consuming-results.md)
- [Operations Guide](operations.md)
- [Network Requirements](reference/network.md)
