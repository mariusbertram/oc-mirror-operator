# TODO-003: Prerequisites Check Could Be Automated

## Metadata
- **Issue ID:** TODO-003
- **Title:** Prerequisites Check Could Be Automated
- **Priority:** P2 (Nice-to-Have)
- **Severity:** Medium
- **Category:** Onboarding Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p2`, `category/onboarding`

## Problem Description

Users must manually verify prerequisites before starting:
- Kubernetes version (≥1.26)
- OLM availability (for OLM installation)
- Registry connectivity (source and target)
- Credential format validity
- Required CRDs existence

This leads to **late failures** - users spend time on setup only to fail later due to missing prerequisites.

## Impact
- **User Impact:** Medium - Users waste time on failed setups
- **Business Impact:** Medium - Increases support requests for basic issues
- **Frequency:** Medium - Affects users who don't read docs carefully

## Evidence

From `docs/quickstart.md`:
```bash
# Check Kubernetes version
kubectl version --client=false | grep Server

# Check if OLM is available
kubectl get crd catalogsources.operators.coreos.com &>/dev/null && echo "✅ OLM installed" || echo "⚠️  OLM not found"

# Check if you have a registry to push to
echo "Registry endpoint: ${REGISTRY:-not set}"
```

These checks are manual and users may skip them.

## Recommended Solution

### 1. Create a Pre-Flight Check CLI Tool

Create `cmd/mirrorctl/main.go` with a `check` subcommand:

```bash
# Install mirrorctl
make build-mirrorctl

# Run pre-flight checks
mirrorctl check

# Output:
✅ Kubernetes version: v1.28.2 (minimum: v1.26.0)
✅ CRDs installed: MirrorTarget, ImageSet, MirrorExport
⚠️  OLM not available (required for OLM installation method)
❌ Cannot connect to registry.example.com:5000 (connection refused)
✅ Credential secret exists: registry-creds
⚠️  Credential format: unknown (should be kubernetes.io/dockerconfigjson)

# Detailed check for specific registry
mirrorctl check registry registry.redhat.io
mirrorctl check registry registry.example.com:5000

# Check specific ImageSet configuration
mirrorctl check imageset my-imageset.yaml
```

### 2. Add Pre-Flight Check to Operator

Add a `PreFlightCheck` field to MirrorTarget:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-target
spec:
  registry: registry.example.com/mirror
  authSecret: registry-creds
  preFlightCheck: true  # Default: true
```

When enabled, the operator:
1. Validates credential secret exists and has correct format
2. Tests connectivity to target registry
3. Tests connectivity to source registries (if specified in ImageSets)
4. Reports any issues in MirrorTarget status conditions

### 3. Create a Pre-Flight Check Job

For users who want to check before installing the operator:

```bash
kubectl apply -f https://raw.githubusercontent.com/mariusbertram/oc-mirror-operator/main/config/preflight-check.yaml

# Check results
kubectl get job preflight-check -o jsonpath='{.status.succeeded}'
kubectl logs job/preflight-check
```

### 4. Add to Documentation

Update all quick start guides to include:
```bash
# Before you begin, run the pre-flight check
mirrorctl check

# Or use the pre-flight check job
kubectl apply -f config/preflight-check.yaml
```

## Acceptance Criteria

✅ Users can run a single command to check all prerequisites
✅ Pre-flight check validates:
  - Kubernetes version
  - CRD installation
  - Registry connectivity (source and target)
  - Credential format
  - OLM availability (if using OLM)
✅ Pre-flight check provides clear, actionable error messages
✅ Pre-flight check is integrated into operator (optional)
✅ Documentation includes pre-flight check instructions

## Implementation Steps

1. **Create `cmd/mirrorctl/main.go`** (2 days)
   - Implement `check` subcommand
   - Add registry connectivity tests
   - Add credential validation
   - Add Kubernetes version check

2. **Create `config/preflight-check.yaml`** (0.5 day)
   - Job that runs pre-flight checks
   - Reports results in status

3. **Add PreFlightCheck to MirrorTarget** (1 day)
   - Add field to API
   - Implement validation in controller
   - Add status conditions

4. **Update documentation** (0.5 day)
   - Add pre-flight check instructions to all quick starts
   - Document mirrorctl tool

## Estimated Effort
- **Total:** 4-5 days
- **Complexity:** Medium
- **Dependencies:** None

## Related Issues
- TODO-001: Too Many Choices for Beginners
- TODO-004: Credential Setup is Confusing
- TODO-017: NetworkPolicy Issues Cause Silent Failures

## Success Metrics
- Number of support requests for basic setup issues: Reduce by 30%
- Time spent troubleshooting prerequisites: Reduce by 50%
- First-time success rate: Improve by 15%

## Notes
This would be a nice addition but has lower priority than the critical P0 issues. Consider implementing as part of the mirrorctl tool (TODO-005 also mentions a CLI tool).
