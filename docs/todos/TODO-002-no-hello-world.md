# TODO-002: No "Hello World" Example

## Metadata
- **Issue ID:** TODO-002
- **Title:** No "Hello World" Example
- **Priority:** P0 (Critical)
- **Severity:** High
- **Category:** Onboarding Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p0`, `category/onboarding`

## Problem Description

There is no minimal working example that demonstrates the most basic functionality. The simplest examples in the documentation still require:
- Understanding of complex concepts (ImageSet, MirrorTarget, channels)
- Access to Red Hat registries (registry.redhat.io)
- Proper credential setup
- Understanding of OpenShift-specific concepts

This means **users cannot see immediate results** and must invest significant time before seeing any success.

## Impact
- **User Impact:** High - Beginners cannot quickly verify the operator works
- **Business Impact:** High - Increases time-to-value and abandonment rate
- **Frequency:** High - Affects every new user

## Evidence

From `docs/quickstart-operator.md`, the simplest example still requires:
1. Installing the operator (OLM or manifests)
2. Creating registry credentials for registry.redhat.io
3. Creating an ImageSet with operator catalog configuration
4. Creating a MirrorTarget
5. Understanding OLM concepts

From `docs/quickstart-release.md`, requires:
- Understanding Cincinnati graph
- Understanding OpenShift release channels
- Access to Red Hat release images

## Recommended Solution

### Create a "5-Minute Quick Start" Guide

Create `docs/quickstart-5minute.md` with:

```yaml
# Minimal configuration that mirrors nginx:latest to a local registry

# 1. Install operator (simplified)
kubectl apply -f https://raw.githubusercontent.com/mariusbertram/oc-mirror-operator/main/config/deploy-simple.yaml

# 2. Create a local registry (if not exists)
docker run -d -p 5000:5000 --name registry registry:2

# 3. Create minimal ImageSet
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: hello-world
  namespace: default
spec:
  mirror:
    additionalImages:
      - name: docker.io/library/nginx:latest

# 4. Create minimal MirrorTarget
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: local-registry
  namespace: default
spec:
  registry: localhost:5000/mirror
  insecure: true
  imageSets: [hello-world]

# 5. Verify
kubectl get imageset hello-world -w
# Watch as nginx:latest is mirrored to localhost:5000/mirror
```

### Key Requirements for Hello World Example

1. **No Red Hat registry access required** - Use public images (nginx, alpine, etc.)
2. **No credential setup required** - Use public images and local registry
3. **Minimal configuration** - <10 lines of YAML total
4. **Immediate feedback** - Should complete in <2 minutes
5. **Clear success criteria** - "You know it works when you see..."

### Add to README

Prominently feature in README.md:

```markdown
## 🚀 5-Minute Quick Start

Want to try oc-mirror-operator right now? This example mirrors nginx:latest to a local registry:

[**Start Here →**](docs/quickstart-5minute.md)

**What you'll need:**
- Kubernetes cluster (Minikube, Kind, or any cluster)
- kubectl configured
- Docker (for local registry)

**Time to complete:** 5 minutes
```

## Acceptance Criteria

✅ A user can complete the example in <5 minutes
✅ No Red Hat credentials or subscriptions required
✅ No OpenShift-specific knowledge required
✅ User sees clear indication of success
✅ Example works on any Kubernetes cluster (≥1.26)
✅ Example uses only public, freely accessible images

## Implementation Steps

1. **Create `docs/quickstart-5minute.md`** (1 day)
   - Write the minimal example
   - Include prerequisites check
   - Add troubleshooting for common issues
   - Test on fresh Minikube cluster

2. **Create simplified deployment manifest** (0.5 day)
   - `config/deploy-simple.yaml` with minimal dependencies
   - Single YAML file for easy installation
   - No OLM dependencies for basic testing

3. **Update README.md** (0.5 day)
   - Add prominent 5-minute quick start section
   - Link to the guide
   - Update navigation

4. **Test the example** (0.5 day)
   - Verify on Minikube
   - Verify on Kind
   - Document any issues

## Estimated Effort
- **Total:** 2-3 days
- **Complexity:** Low
- **Dependencies:** None

## Related Issues
- TODO-001: Too Many Choices for Beginners (this helps solve that)
- TODO-004: Credential Setup is Confusing (this avoids credentials entirely)
- TODO-005: YAML Configuration is Error-Prone (minimal YAML reduces errors)

## Success Metrics
- Time to first successful mirror: <5 minutes (currently 30-60+ minutes)
- Percentage of users who try the 5-minute guide: >70%
- First-time success rate for basic mirroring: >90%

## Notes
This is the single most important UX improvement. A working "hello world" example that works quickly and reliably will dramatically improve adoption and user satisfaction.
