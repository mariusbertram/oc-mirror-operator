# Quick Start: Full Disconnected Cluster Setup  
**Use case:** Complete setup for a fully disconnected (air-gapped) OpenShift cluster, including releases, operators, and Helm charts.

---

## Overview

This guide walks you through setting up a **complete mirror** for a disconnected OpenShift cluster, including:

1. **OpenShift releases** (with all component images)
2. **Operator catalogs** (filtered to your selected packages)
3. **Helm charts** (optional)
4. **Additional images** (custom images you need)
5. **Release signatures** (required for cluster upgrades)
6. **Mirror rules** (IDMS/ITMS for image redirection)

---

## Prerequisites

- A **connected cluster** (with internet access) for mirroring
- A **target registry** (Quay, Harbor, registry:2, etc.) accessible from both clusters
- **Sufficient storage** in your registry (OpenShift releases require ~20-50GB per version)
- **Cluster admin privileges** on both clusters

---

## Step 1: Install the Operator on the Connected Cluster

Follow the installation steps from [Quick Start: Single Operator](quickstart-operator.md#1-install-the-operator).

**Recommendation:** Use OLM installation for OpenShift, or plain manifests for Kubernetes.

---

## Step 2: Prepare Registry Credentials

Create credentials for both **source registries** (registry.redhat.io, quay.io) and your **target registry**:

```bash
kubectl create namespace oc-mirror-operator

# Create secret with all required credentials
kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${HOME}/.docker/config.json \
  --type=kubernetes.io/dockerconfigjson -n oc-mirror-operator

# Verify the secret
kubectl get secret registry-creds -n oc-mirror-operator
```

**Note:** For details on the four required registry hosts, see [Credentials Configuration](configuration/credentials.md).

---

## Step 3: Create a Comprehensive ImageSet

Create an `ImageSet` that includes all the content you need for your disconnected cluster:

```yaml
# disconnected-imageset.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: disconnected-cluster
  namespace: oc-mirror-operator
spec:
  mirror:
    # OpenShift releases
    platform:
      architectures: [amd64]  # Add arm64, s390x, ppc64le as needed
      channels:
        - name: stable-4.16
          minVersion: "4.16.0"
          maxVersion: "4.16.30"
      graph: true  # For OSUS graph data
      kubeVirtContainer: true  # For OpenShift Virtualization
    
    # Operator catalogs
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal
          - name: openshift-pipelines-operator-rh
          - name: cluster-logging
          - name: openshift-monitoring
        # Optional: Include dependencies
        # skipDependencies: false  # Default is true (include dependencies)
    
    # Helm charts (optional)
    helm:
      repositories:
        - name: bitnami
          url: https://charts.bitnami.com/bitnami
          charts:
            - name: nginx
            - name: redis
    
    # Additional images
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
      - name: quay.io/prometheus/prometheus:v2.48.0
```

Apply the ImageSet:
```bash
kubectl apply -f disconnected-imageset.yaml
```

**Customize for your environment:**
- Adjust the OpenShift **version range** (`minVersion`, `maxVersion`)
- Add/remove **operator packages** as needed
- Add/remove **Helm charts** as needed
- Add any **additional images** your applications require

---

## Step 4: Create the MirrorTarget

```yaml
# disconnected-mirrortarget.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: disconnected-registry
  namespace: oc-mirror-operator
  annotations:
    # Enable cleanup to automatically remove orphaned images
    mirror.openshift.io/cleanup-policy: Delete
spec:
  registry: registry.example.com/disconnected  # Your registry path
  authSecret: registry-creds
  imageSets:
    - disconnected-cluster
  
  # Performance tuning (adjust based on your registry)
  concurrency: 3  # 1 for Quay, 3-5 for Harbor/Nexus
  batchSize: 50
  
  # Polling configuration
  pollInterval: 24h  # Check for new versions daily
  checkExistInterval: 6h  # Verify images exist every 6 hours
  
  # Retry configuration
  maxRetries: 10
  
  # Expose the Resource API for easy access
  expose:
    type: Route  # Use Ingress for Kubernetes
    host: mirror.apps.example.com  # Optional custom host
```

Apply the MirrorTarget:
```bash
kubectl apply -f disconnected-mirrortarget.yaml
```

---

## Step 5: Monitor the Mirroring Process

This will take **some time** depending on:
- Number of releases/operators/charts
- Size of the images
- Network bandwidth
- Registry performance

```bash
# Watch overall progress
kubectl get mirrortarget,imageset -n oc-mirror-operator -w

# View detailed status
kubectl get mirrortarget disconnected-registry -n oc-mirror-operator -o json | jq '.status'

# View ImageSet status
kubectl get imageset disconnected-cluster -n oc-mirror-operator -o json | jq '.status'

# View manager logs
kubectl logs deployment/disconnected-registry-manager -n oc-mirror-operator -f

# View worker pods
kubectl get pods -n oc-mirror-operator -l app=oc-mirror-worker -w
```

**Expected progress:**
- OpenShift releases: ~190 images per version × number of versions
- Operators: Varies by package (typically 5-50 images per operator)
- Helm charts: Varies by chart complexity
- Additional images: As specified

---

## Step 6: Verify the Mirror

Once all images show as `Mirrored` (and `CatalogReady` is `True`), verify the content in your registry:

```bash
# List all repositories
skopeo list-tags docker://registry.example.com/disconnected

# Check release images
skopeo list-tags docker://registry.example.com/disconnected/openshift/release-images

# Check operator catalog
skopeo list-tags docker://registry.example.com/disconnected/redhat/redhat-operator-index

# Check Helm chart images
skopeo list-tags docker://registry.example.com/disconnected/bitnami
```

---

## Step 7: Collect Mirror Artifacts

You need to collect several artifacts from the mirror to configure your disconnected cluster:

### 1. ImageDigestMirrorSet (IDMS) and ImageTagMirrorSet (ITMS)

```bash
# Get the Resource API URL
URL=https://$(kubectl get route disconnected-registry-resources -n oc-mirror-operator -o jsonpath='{.spec.host}')

# Get IDMS
curl -sk $URL/api/v1/targets/disconnected-registry/imagesets/disconnected-cluster/idms.yaml -o idms.yaml

# Get ITMS
curl -sk $URL/api/v1/targets/disconnected-registry/imagesets/disconnected-cluster/itms.yaml -o itms.yaml

# For Kubernetes (no Route), use port-forward:
kubectl port-forward svc/oc-mirror-resource-api 8081:8081 -n oc-mirror-operator &
URL=http://localhost:8081
```

### 2. Release Signatures

```bash
# Get signatures
curl -sk $URL/api/v1/targets/disconnected-registry/signatures.yaml -o signatures.yaml
```

### 3. CatalogSource

```bash
# Get CatalogSource for each operator catalog
curl -sk $URL/api/v1/targets/disconnected-registry/imagesets/disconnected-cluster/catalogs/redhat-operator-index-v4.16/catalogsource.yaml -o catalogsource.yaml
```

### 4. ClusterCatalog (Optional, for OLM v1)

```bash
curl -sk $URL/api/v1/targets/disconnected-registry/imagesets/disconnected-cluster/catalogs/redhat-operator-index-v4.16/clustercatalog.yaml -o clustercatalog.yaml
```

---

## Step 8: Transfer Artifacts to the Disconnected Cluster

You have several options for transferring the artifacts:

### Option A: Direct Network Transfer (Recommended)

If the disconnected cluster can access the Resource API:

```bash
# On the disconnected cluster, apply the artifacts directly
kubectl apply -f idms.yaml
kubectl apply -f itms.yaml
kubectl apply -f signatures.yaml
kubectl apply -f catalogsource.yaml
```

### Option B: Manual File Transfer

Copy the files to the disconnected cluster and apply them:

```bash
# On the connected cluster
scp idms.yaml itms.yaml signatures.yaml catalogsource.yaml user@disconnected-cluster:

# On the disconnected cluster
kubectl apply -f idms.yaml
kubectl apply -f itms.yaml
kubectl apply -f signatures.yaml
kubectl apply -f catalogsource.yaml
```

### Option C: Using MirrorExport (For Air-Gap)

For truly air-gapped environments, use `MirrorExport`:

```yaml
# disconnected-export.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorExport
metadata:
  name: disconnected-export
  namespace: oc-mirror-operator
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.0"
          maxVersion: "4.16.30"
      kubeVirtContainer: true
      graph: true
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal
          - name: openshift-pipelines-operator-rh
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
  destination:
    registry: registry.example.com/disconnected
```

Apply and wait for completion:
```bash
kubectl apply -f disconnected-export.yaml
kubectl get mirrorexport disconnected-export -n oc-mirror-operator -w
```

Get the artifacts:
```bash
kubectl get cm disconnected-export-artifacts -n oc-mirror-operator -o jsonpath='{.data.manifest\.json}' > manifest.json
kubectl get cm disconnected-export-artifacts -n oc-mirror-operator -o jsonpath='{.data.buildspec\.json}' > buildspec.json
```

---

## Step 9: Configure the Disconnected Cluster

### 1. Disable Default CatalogSources

On OpenShift, disable the default Red Hat catalogs:

```bash
kubectl patch operatorhub cluster --type merge -p '{"spec":{"disableAllDefaultSources":true}}'
```

### 2. Apply Mirror Rules

```bash
# Apply IDMS and ITMS
kubectl apply -f idms.yaml
kubectl apply -f itms.yaml
```

### 3. Apply Release Signatures

```bash
kubectl apply -f signatures.yaml
```

### 4. Apply CatalogSource

```bash
kubectl apply -f catalogsource.yaml
```

### 5. Configure Registry Trust

Ensure the disconnected cluster trusts your registry's CA certificate:

```bash
# Create a ConfigMap with your registry's CA
kubectl create configmap registry-ca --from-file=ca-bundle.crt=/path/to/ca.crt -n openshift-config

# Update the cluster's additional trusted CA
kubectl patch proxy/cluster --type merge -p '{"spec":{"trustedCA":{"name":"registry-ca"}}}'
```

### 6. Create Pull Secret for the Registry

```bash
# Create a pull secret for your registry
kubectl create secret docker-registry disconnected-registry-creds \
  --docker-server=registry.example.com \
  --docker-username=<username> \
  --docker-password=<password> \
  --docker-email=<email> \
  -n openshift-config

# Add the secret to the cluster's pull secrets
kubectl patch secret/pull-secret -n openshift-config --type merge -p '{"data":{"\.dockerconfigjson":"'$(kubectl get secret disconnected-registry-creds -n openshift-config -o jsonpath='{.data.\.dockerconfigjson}')'"}}'
```

---

## Step 10: Verify the Disconnected Cluster

### 1. Check Image Mirroring

```bash
# Check if images are being redirected
kubectl get idms,itms

# Test pulling a mirrored image
podman pull registry.example.com/disconnected/openshift/release-images:4.16.20-x86_64
```

### 2. Check Operator Catalog

```bash
# Check if the CatalogSource is ready
kubectl get catalogsource -n openshift-marketplace

# Check if operators are available
kubectl get packages -n openshift-marketplace
```

### 3. Test Cluster Upgrade

```bash
# Check available updates
oc adm upgrade

# The cluster should see the mirrored releases
```

### 4. Install an Operator

```bash
# Create an OperatorGroup
kubectl create namespace my-operators
cat <<EOT | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: my-operators
  namespace: my-operators
spec:
  targetNamespaces: [my-operators]
EOT

# Install an operator
cat <<EOT | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: web-terminal
  namespace: my-operators
spec:
  name: web-terminal
  channel: stable
  source: redhat-operator-index-v4-16
  sourceNamespace: openshift-marketplace
EOT
```

---

## Step 11: Set Up Continuous Mirroring (Optional)

To keep your mirror up-to-date:

### 1. Enable Periodic Polling

The MirrorTarget already has `pollInterval: 24h` by default, which checks for new versions daily.

### 2. Enable Drift Detection

The MirrorTarget already has `checkExistInterval: 6h` by default, which verifies images exist in the target registry.

### 3. Set Up Monitoring

The operator provides Prometheus metrics. Set up monitoring:

```bash
# Check if ServiceMonitors were created
kubectl get servicemonitors -n oc-mirror-operator

# View metrics
kubectl get --raw /api/v1/namespaces/oc-mirror-operator/services/oc-mirror-operator-controller-manager:9090/proxy/metrics
```

---

## Troubleshooting Disconnected Setup

| Issue | Solution |
|---|---|
| **Images not found on disconnected cluster** | Verify IDMS/ITMS are applied and the registry is accessible |
| **CatalogSource not showing operators** | Check if the CatalogSource is in `Ready` state and the filtered catalog image exists |
| **Cluster upgrade fails** | Ensure release signatures are applied and the cluster can access the mirror registry |
| **Operators not installable** | Check if all required images are mirrored (use `kubectl get imageset -o json | jq '.status.failedImageDetails'`) |
| **Authentication errors on disconnected cluster** | Verify the pull secret is correctly configured in the cluster's `pull-secret` |
| **Registry certificate errors** | Ensure the registry's CA certificate is trusted by the cluster |

For more troubleshooting, see:
- [Troubleshooting Guide](troubleshooting.md)
- [Resource API / Console Plugin Problems](troubleshooting.md#resource-api--console-plugin-problems)

---

## Maintenance Tasks

### 1. Add New Content

To add new operators, releases, or charts:

```bash
# Edit the ImageSet
kubectl edit imageset disconnected-cluster -n oc-mirror-operator

# The operator will automatically start mirroring the new content
```

### 2. Remove Old Content

To remove content that's no longer needed:

```bash
# Edit the ImageSet to remove the content
kubectl edit imageset disconnected-cluster -n oc-mirror-operator

# If cleanup-policy is set to Delete, orphaned images will be removed
# Otherwise, they remain in the registry
```

### 3. Update to New OpenShift Versions

To add new OpenShift versions:

```bash
# Edit the ImageSet to include the new version range
kubectl edit imageset disconnected-cluster -n oc-mirror-operator

# Example: Add 4.17 releases
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16
          minVersion: "4.16.0"
          maxVersion: "4.16.30"
        - name: stable-4.17
          minVersion: "4.17.0"
```

### 4. Monitor Mirror Health

```bash
# Check MirrorTarget status
kubectl get mirrortarget disconnected-registry -n oc-mirror-operator -o json | jq '.status.conditions'

# Check for failed images
kubectl get imageset disconnected-cluster -n oc-mirror-operator -o json | jq '.status.failedImageDetails'

# View manager logs for errors
kubectl logs deployment/disconnected-registry-manager -n oc-mirror-operator --tail=100
```

---

## Next Steps

- [Operations Guide](operations.md) - Day-2 operations
- [Monitoring](reference/metrics.md) - Set up monitoring and alerts
- [Upgrading the Operator](upgrades.md) - Upgrade the operator itself
- [Architecture](architecture.md) - Understand how it all works

---

## See Also

- [Getting Started](getting-started.md)
- [Concepts](concepts.md)
- [Configuration Reference](configuration/imagesets.md)
- [Consuming Results](consuming-results.md)
- [Troubleshooting](troubleshooting.md)
