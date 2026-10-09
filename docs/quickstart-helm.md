# Quick Start: Mirror Helm Charts  
**Use case:** Mirror Helm charts (including their container images) from HTTP/HTTPS repositories or OCI registries to your private registry.

---

## 1. Install the Operator

Follow the same installation steps as in [Quick Start: Single Operator](quickstart-operator.md#1-install-the-operator).

---

## 2. Prepare Registry Credentials

Follow the same credential setup as in [Quick Start: Single Operator](quickstart-operator.md#2-prepare-registry-credentials).

**Note:** For Helm chart mirroring, you typically only need **push** credentials for your target registry, as Helm repositories are usually publicly accessible.

---

## 3. Create the ImageSet for Helm Charts

An `ImageSet` for Helm charts specifies which **repositories** and **charts** to mirror:

```yaml
# helm-charts-imageset.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: helm-charts
  namespace: oc-mirror-operator
spec:
  mirror:
    helm:
      repositories:
        - name: bitnami
          url: https://charts.bitnami.com/bitnami
          charts:
            - name: nginx
              version: "15.5.1"  # Specific version
            - name: redis
              # No version = latest non-prerelease
            - name: postgresql
              version: "14.0.0"
```

Apply the ImageSet:
```bash
kubectl apply -f helm-charts-imageset.yaml
```

**Customize for your use case:**

### HTTP/HTTPS Repositories
Most Helm repositories use HTTP/HTTPS with an `index.yaml`:

```yaml
repositories:
  - name: prometheus-community
    url: https://prometheus-community.github.io/helm-charts
    charts:
      - name: prometheus
      - name: grafana
```

### OCI Repositories
For OCI-based Helm repositories (like Bitnami's OCI registry):

```yaml
repositories:
  - name: bitnami-oci
    url: oci://registry-1.docker.io/bitnamicharts
    charts:
      - name: nginx
        version: "15.5.1"  # Version is required for OCI repos
      - name: redis
        version: "18.0.0"
```

**Important:** For OCI repositories, the `version` field is **required** because there's no index to query for the latest version.

### Custom Image Paths
Some Helm charts reference images in non-standard locations. Use `imagePaths` to specify additional JSONPath expressions:

```yaml
repositories:
  - name: my-repo
    url: https://my-repo.example.com
    charts:
      - name: my-chart
        version: "1.0.0"
        imagePaths:
          - "{.spec.template.spec.containers[?(@.name==\"app\")].image}"
          - "{.spec.initContainers[*].image}"
```

---

## 4. Create the MirrorTarget

```yaml
# helm-charts-mirrortarget.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-registry
  namespace: oc-mirror-operator
spec:
  registry: registry.example.com/helm-mirror  # Your registry path
  authSecret: registry-creds
  imageSets:
    - helm-charts
```

Apply the MirrorTarget:
```bash
kubectl apply -f helm-charts-mirrortarget.yaml
```

---

## 5. Watch the Mirroring Process

```bash
# Watch progress
kubectl get mirrortarget,imageset -n oc-mirror-operator -w

# Example output:
# NAME                          TOTAL   MIRRORED   PENDING   FAILED   AGE
# mirrortarget.my-registry       8      8         0         0        3m
# imageset.helm-charts           8      8         0         0        3m
```

View logs:
```bash
# Manager logs
kubectl logs deployment/my-registry-manager -n oc-mirror-operator -f

# Worker logs
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker
kubectl logs <worker-pod-name> -n oc-mirror-operator -f
```

---

## 6. Verify the Mirror

Once mirroring is complete, verify the Helm chart images in your registry:

```bash
# List all mirrored images
skopeo list-tags docker://registry.example.com/helm-mirror

# Check specific chart images
skopeo list-tags docker://registry.example.com/helm-mirror/bitnami/nginx

# Check Helm chart packages (OCI artifacts)
skopeo list-tags docker://registry.example.com/helm-mirror/charts/bitnami/nginx
```

**Note:** The operator mirrors both the **container images referenced by the Helm charts** and the **Helm chart packages themselves** as OCI artifacts to `registry/helm-mirror/charts/repo/chart:version`.

---

## 7. Use the Mirrored Charts

To use the mirrored Helm charts in a disconnected environment:

### Option A: Use Helm with `--repo` (for HTTP/HTTPS repos)

If the original Helm repository is accessible from your disconnected cluster:

```bash
# Add the repository
helm repo add bitnami https://charts.bitnami.com/bitnami
helm repo update

# Install with image overrides
helm install my-nginx bitnami/nginx \
  --set image.repository=registry.example.com/helm-mirror/bitnami/nginx \
  --set image.tag=15.5.1
```

### Option B: Install from Mirrored Charts

The operator automatically mirrors the Helm chart packages as OCI artifacts. You can install directly from the mirror:

```bash
# Install directly from the mirrored OCI repository
helm install my-nginx oci://registry.example.com/helm-mirror/charts/bitnami --version 15.5.1
```

### Option C: Use ImageDigestMirrorSet

Apply the generated IDMS to redirect image pulls:

```bash
# Get the IDMS
URL=https://$(kubectl get route my-registry-resources -n oc-mirror-operator -o jsonpath='{.spec.host}')
curl -sk $URL/api/v1/targets/my-registry/imagesets/helm-charts/idms.yaml

# Apply to your cluster
curl -sk $URL/api/v1/targets/my-registry/imagesets/helm-charts/idms.yaml | kubectl apply -f -
```

---

## 8. Advanced Configuration

### Mirror Specific Chart Versions

List desired versions explicitly:

```yaml
repositories:
  - name: bitnami
    url: https://charts.bitnami.com/bitnami
    charts:
      - name: nginx
        version: "15.5.0"
      - name: nginx
        version: "15.5.1"
```

**Tip:** For HTTP/HTTPS repositories, omit `version:` to mirror the latest non-prerelease version.

---

## Troubleshooting

| Issue | Solution |
|---|---|
| **Chart not found** | Verify the chart exists in the repository: `helm search repo <repo-name>/<chart-name>` |
| **Version not found** | For OCI repos, ensure you specified a version. For HTTP repos, check the chart has that version. |
| **Images not extracted** | The chart might reference images in non-standard locations. Use `imagePaths` to specify additional paths. |
| **Authentication errors** | For private Helm repositories, you need to configure access. Currently, the operator only supports public HTTP/HTTPS repos. |
| **Chart download timeout** | Large charts might timeout. Check manager logs for details. |
| **Too many images** | Helm charts can reference many images. Consider mirroring specific versions only. |

For more troubleshooting, see [Troubleshooting Guide](troubleshooting.md).

---

## Performance Tips

| Scenario | Recommendation |
|---|---|
| **Large Helm repositories** | Mirror specific charts/versions, not all charts |
| **Many chart versions** | List desired versions explicitly to limit the range |
| **Slow chart downloads** | The operator downloads and renders each chart, which can be slow for complex charts |
| **Private HTTP/HTTPS repos** | Currently not supported. Use public repos or mirror the charts manually first. |
| **Private OCI repos** | Supported via the MirrorTarget's `authSecret` credentials. |

---

## Next Steps

- [Mirror OpenShift releases](quickstart-release.md) - Add release mirroring
- [Mirror a single operator](quickstart-operator.md) - Add operator mirroring
- [Full disconnected setup](quickstart-disconnected.md) - Complete air-gapped setup
- [Advanced Helm configuration](configuration/imagesets.md#helm-charts)

---

## See Also

- [ImageSet Configuration: Helm](configuration/imagesets.md#helm-charts)
- [MirrorTarget Configuration](configuration/mirrortarget.md)
- [Consuming Results](consuming-results.md)
- [Operations Guide](operations.md)
