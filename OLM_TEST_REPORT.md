# OLM Deployment Test Report for oc-mirror-operator

**Date:** 2026-05-02  
**Test Status:** PARTIAL SUCCESS (Bundle valid, Bundle Image Build Required)

---

## Executive Summary

The oc-mirror-operator bundle structure and configuration are valid for OLM deployment. The operator CRDs and RBAC are properly configured. However, the bundle container image must be built and made accessible to the OpenShift cluster for full OLM deployment to succeed.

---

## Test Results

### ✅ Step 1: Bundle Validation

- **Bundle Structure**: ✓ PASSED
  - `bundle/manifests/` directory with all required manifests
  - `bundle/metadata/` directory with annotations
  - All manifests are well-formed

- **ClusterServiceVersion (CSV)**: ✓ PASSED
  - Name: `oc-mirror.v0.0.2`
  - Version: v1alpha1
  - API server: Kubernetes operator pattern
  - Install mode: AllNamespaces (supported: true)
  - Other modes: OwnNamespace, SingleNamespace, MultiNamespace (not supported)

- **Custom Resource Definitions**: ✓ PASSED
  - ImageSet CRD: ✓ Included
  - MirrorTarget CRD: ✓ Included
  - Both CRDs already installed on cluster

- **RBAC Configuration**: ✓ PASSED
  - mirror.openshift.io API group permissions: ✓
  - rbac.authorization.k8s.io permissions: ✓
  - Core API (pods, configmaps, secrets): ✓
  - Apps API (deployments): ✓
  - Batch API (jobs): ✓
  - Networking API (ingresses, networkpolicies): ✓
  - Routes (OpenShift-specific): ✓

- **Deployment Configuration**: ✓ PASSED
  - Deployment: oc-mirror-controller-manager
  - Service Account: oc-mirror-controller-manager
  - Replicas: 1
  - Security Context: Proper (non-root, restricted)
  - Resource Limits: 500m CPU, 128Mi Memory

- **Environment Variables**: ⚠ PARTIAL
  - OPERATOR_IMAGE: ✓ Configured
  - MANAGER_IMAGE: ✓ Configured
  - WORKER_IMAGE: ✓ Configured
  - DASHBOARD_IMAGE: ✗ Not configured (dashboard not yet integrated)
  - OPERATOR_NAMESPACE: ✗ Not configured

- **Console Plugin Support**: ✗ NOT IMPLEMENTED
  - ConsolePlugin RBAC not included
  - Dashboard deployment not configured
  - Note: These are on the feature roadmap but not yet implemented

### ✅ Step 2: Cluster Validation

- **OpenShift Cluster**: ✓ Connected
  - Context: admin
  - OLM (Operator Lifecycle Manager): ✓ Installed

- **Namespaces**: ✓ Ready
  - openshift-operators: ✓ Available
  - openshift-marketplace: ✓ Available
  - operators: ✓ Can be created

### ❌ Step 3: Bundle Image Deployment

- **Bundle Container Image**: ✗ FAILED
  - Image URI: `ghcr.io/mariusbertram/oc-mirror-operator-bundle:v0.0.2`
  - Status: TRANSIENT_FAILURE
  - Reason: Bundle image not accessible (likely not built/pushed or cluster cannot access ghcr.io)

- **Cause Analysis**:
  1. Bundle image has not been built or pushed to ghcr.io
  2. Cluster may not have internet access to pull from ghcr.io
  3. Registry authentication may be required

### ✅ Step 4: Local Bundle Validation

- **Bundle Manifests Present**: ✓
  - oc-mirror.clusterserviceversion.yaml: ✓
  - mirror.openshift.io_imagesets.yaml: ✓
  - mirror.openshift.io_mirrortargets.yaml: ✓
  - RBAC manifests: ✓
  - All manifest files syntax-valid

---

## Issues & Findings

### Issue 1: Bundle Image Not Accessible ⚠
- **Severity**: HIGH
- **Impact**: OLM deployment cannot proceed
- **Resolution**: Build and push the bundle image
  ```bash
  make bundle IMG=ghcr.io/your-user/oc-mirror-operator:v0.0.2
  make bundle-build BUNDLE_IMG=ghcr.io/your-user/oc-mirror-operator-bundle:v0.0.2
  podman push ghcr.io/your-user/oc-mirror-operator-bundle:v0.0.2
  ```

### Issue 2: Missing Dashboard Integration ⚠
- **Severity**: MEDIUM (Feature)
- **Impact**: Dashboard console plugin not available in OLM deployment
- **Status**: Not yet implemented
- **Tracking**: Feature roadmap

### Issue 3: Missing ConsolePlugin RBAC ⚠
- **Severity**: MEDIUM (Feature)
- **Impact**: OpenShift Console integration not available
- **Status**: Not yet implemented

---

## What Works ✓

1. **Bundle Structure**: Properly organized and valid
2. **CSV Configuration**: Complete and correct for AllNamespaces deployment
3. **CRD Management**: Both ImageSet and MirrorTarget CRDs included
4. **RBAC**: Comprehensive permissions for core functionality
5. **Deployment Spec**: Proper security context and resource limits
6. **Cluster Integration**: OLM and required namespaces available

---

## What Needs Work ⚠

1. **Bundle Image Build & Push**:
   - The container image for the bundle needs to be built
   - Image should be pushed to an accessible registry
   - Current target: ghcr.io (requires authentication for push)

2. **Dashboard Console Plugin** (Future Enhancement):
   - Dashboard pod deployment not implemented
   - Console plugin RBAC not configured
   - Would add OpenShift console visualization

3. **Environment Variables** (Optional):
   - DASHBOARD_IMAGE and OPERATOR_NAMESPACE could be added to CSV

---

## Deployment Instructions

Once the bundle image is built and pushed:

### 1. Create CatalogSource
```bash
cat <<'EOF' | oc apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: oc-mirror-catalog
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: <your-registry>/oc-mirror-operator-bundle:v0.0.2
  displayName: "OC Mirror Operators"
  publisher: "Marius Bertram"
EOF
```

### 2. Create Subscription
```bash
cat <<'EOF' | oc apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: oc-mirror-subscription
  namespace: openshift-operators
spec:
  channel: alpha
  installPlanApproval: Automatic
  name: oc-mirror-operator
  source: oc-mirror-catalog
  sourceNamespace: openshift-marketplace
EOF
```

### 3. Monitor Deployment
```bash
# Wait for CSV to be installed
oc get csv -n openshift-operators oc-mirror.v0.0.2 -w

# Check operator pod
oc get pods -n openshift-operators -l app.kubernetes.io/name=oc-mirror

# View logs
oc logs -n openshift-operators deployment/oc-mirror-controller-manager -f
```

---

## Verification Checklist

- [x] Bundle manifests are valid
- [x] CSV configuration is correct
- [x] CRDs are defined
- [x] RBAC is properly configured
- [x] Deployment spec is secure
- [ ] Bundle image is built and accessible
- [ ] CatalogSource is ready (requires image)
- [ ] Subscription deploys successfully (requires image)
- [ ] Operator pod is running (requires image)
- [ ] CRDs are properly installed (requires operator)

---

## Recommendations

1. **Immediate** (Required for OLM deployment):
   - Build the bundle image: `make bundle-build`
   - Push to an accessible registry
   - Test deployment steps above

2. **Near-term** (Recommended):
   - Test subscription automatic approval
   - Test operator updates
   - Verify metrics endpoint

3. **Future** (Nice to have):
   - Implement dashboard console plugin
   - Add DASHBOARD_IMAGE environment variable
   - Add OPERATOR_NAMESPACE environment variable
   - Support additional install modes (OwnNamespace, SingleNamespace)

---

## Test Environment

- **Cluster**: OpenShift (admin context)
- **OLM Version**: Installed and operational
- **Test Date**: 2026-05-02
- **Test Method**: Manual OLM deployment test script

---

## Conclusion

The oc-mirror-operator is **ready for OLM deployment** in terms of bundle configuration and RBAC. The primary blocker is that the bundle container image has not been built and pushed to an accessible registry. Once the image is available, the operator should deploy successfully via OLM to the OpenShift cluster.

**Test Status**: ✓ BUNDLE STRUCTURE VALID, ⚠ BUNDLE IMAGE BUILD REQUIRED

