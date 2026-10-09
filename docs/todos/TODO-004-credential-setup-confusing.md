# TODO-004: Credential Setup is Confusing

## Metadata
- **Issue ID:** TODO-004
- **Title:** Credential Setup is Confusing
- **Priority:** P0 (Critical)
- **Severity:** High
- **Category:** Onboarding Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p0`, `category/onboarding`

## Problem Description

The credential setup process presents users with **3 different methods** without clear guidance on which to use:

1. **Method 1:** From existing Docker config (`~/.docker/config.json`)
2. **Method 2:** From OpenShift pull secret
3. **Method 3:** Manual creation with `kubectl create secret docker-registry`

Additionally:
- Users must understand which registries need credentials (source and/or target)
- The secret must have the correct type (`kubernetes.io/dockerconfigjson`)
- The secret must contain credentials for **both** source and target registries
- Different container runtimes (Docker, Podman, containerd) store credentials differently

This leads to **authentication errors** which are one of the most common early failure points.

## Impact
- **User Impact:** High - Authentication errors are a top cause of early failures
- **Business Impact:** High - Increases support burden significantly
- **Frequency:** Very High - Affects almost every user during initial setup

## Evidence

From `docs/quickstart-operator.md`:
```bash
# Method 1: From existing Docker config
export NS=oc-mirror-operator
podman login registry.redhat.io
podman login registry.example.com  # Replace with your target registry

kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${XDG_RUNTIME_DIR}/containers/auth.json \
  --type=kubernetes.io/dockerconfigjson -n $NS

# Method 2: From OpenShift pull secret (if on OpenShift)
kubectl get secret/pull-secret -n openshift-config --export -o yaml | \
  kubectl apply -n oc-mirror-operator -f -

# Method 3: Manual creation
kubectl create secret docker-registry registry-creds \
  --docker-server=registry.redhat.io \
  --docker-username=<your-username> \
  --docker-password=<your-password> \
  --docker-email=<your-email> -n oc-mirror-operator
```

**Problems:**
- No guidance on which method to use when
- Method 3 creates wrong secret type (should be `kubernetes.io/dockerconfigjson`)
- Doesn't explain that **both** source and target credentials are needed
- Doesn't explain how to combine multiple registries in one secret

From `docs/configuration/credentials.md`, the documentation is comprehensive but complex for beginners.

## Recommended Solution

### 1. Create a Credential Setup Decision Tree

Add to `docs/configuration/credentials.md`:

```markdown
## Which Method Should You Use?

| Your Setup | Recommended Method | Notes |
|------------|-------------------|-------|
| Docker Desktop on macOS/Windows | Method 1 (Docker config) | Use `~/.docker/config.json` |
| Podman on Linux | Method 1 (Podman auth) | Use `${XDG_RUNTIME_DIR}/containers/auth.json` or `~/.config/containers/auth.json` |
| OpenShift cluster | Method 2 (pull secret) | Uses cluster's global pull secret |
| Multiple registries | Method 1 + combine | Create secret with credentials for all registries |
| CI/CD pipeline | Method 3 (manual) | Programmatic credential creation |

## Quick Setup

For most users, this single command works:

```bash
# For Docker users
kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${HOME}/.docker/config.json \
  --type=kubernetes.io/dockerconfigjson -n <namespace>

# For Podman users
kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${XDG_RUNTIME_DIR}/containers/auth.json \
  --type=kubernetes.io/dockerconfigjson -n <namespace>
```

**Important:** Your `~/.docker/config.json` must contain credentials for:
- ✅ Source registries (registry.redhat.io, quay.io, etc.)
- ✅ Target registry (your mirror destination)
```

### 2. Create a Credential Helper Tool

Add to `cmd/mirrorctl/main.go`:

```bash
# Create credentials secret interactively
mirrorctl create-credentials

# Output:
? Which container runtime do you use? [Docker/Podman/Other]
? Source registry (e.g., registry.redhat.io): 
? Source username: 
? Source password: 
? Target registry (e.g., registry.example.com): 
? Target username: 
? Target password: 
? Namespace: 
? Secret name [registry-creds]: 

✅ Created secret 'registry-creds' in namespace 'mirror' with credentials for 2 registries

# Or create from existing config
mirrorctl create-credentials --from-docker-config --namespace mirror

# Or validate existing secret
mirrorctl check credentials registry-creds --namespace mirror
```

### 3. Add Credential Validation

The operator should validate credentials on startup and report issues:

```yaml
# In MirrorTarget status
status:
  conditions:
    - type: CredentialsValid
      status: "False"
      reason: MissingCredentials
      message: "Secret 'registry-creds' does not contain credentials for registry.redhat.io"
    - type: CredentialsValid
      status: "False"
      reason: InvalidFormat
      message: "Secret 'registry-creds' has type 'docker-registry' but should be 'kubernetes.io/dockerconfigjson'"
```

### 4. Fix Documentation Issues

In `docs/quickstart-operator.md`, Method 3 is incorrect:
```bash
# WRONG - creates wrong secret type
kubectl create secret docker-registry registry-creds \
  --docker-server=registry.redhat.io \
  --docker-username=<your-username> \
  --docker-password=<your-password> \
  --docker-email=<your-email> -n oc-mirror-operator

# CORRECT - for single registry
kubectl create secret docker-registry registry-creds \
  --docker-server=registry.redhat.io \
  --docker-username=<your-username> \
  --docker-password=<your-password> \
  --docker-email=<your-email> \
  --type=kubernetes.io/dockerconfigjson -n oc-mirror-operator

# BETTER - for multiple registries, use docker-registry type is not ideal
# Use Method 1 or create proper dockerconfigjson manually
```

### 5. Add Multi-Registry Example

Add to `docs/configuration/credentials.md`:

```yaml
# Combining credentials for multiple registries
apiVersion: v1
kind: Secret
metadata:
  name: registry-creds
  namespace: mirror
type: kubernetes.io/dockerconfigjson
stringData:
  .dockerconfigjson: |
    {
      "auths": {
        "registry.redhat.io": {
          "auth": "$(echo -n 'username:password' | base64)"
        },
        "registry.example.com": {
          "auth": "$(echo -n 'username:password' | base64)"
        },
        "quay.io": {
          "auth": "$(echo -n 'username:password' | base64)"
        }
      }
    }
```

## Acceptance Criteria

✅ Clear guidance on which credential method to use for different scenarios
✅ Single recommended command for most users (Method 1)
✅ Explanation that **both** source and target credentials are needed
✅ Credential validation in operator with clear error messages
✅ Helper tool for creating credentials (mirrorctl)
✅ Documentation fixes for incorrect examples
✅ Multi-registry credential example

## Implementation Steps

1. **Update `docs/configuration/credentials.md`** (1 day)
   - Add decision tree
   - Add quick setup section
   - Fix incorrect examples
   - Add multi-registry example

2. **Update `docs/quickstart-operator.md`** (0.5 day)
   - Simplify to show Method 1 primarily
   - Move Methods 2 and 3 to credentials.md
   - Add clear guidance

3. **Add credential validation to operator** (2 days)
   - Check secret exists
   - Check secret type
   - Check credentials for registries in ImageSets
   - Report in status conditions

4. **Create mirrorctl credential helper** (1 day)
   - Implement `create-credentials` subcommand
   - Implement `check credentials` subcommand

## Estimated Effort
- **Total:** 4-5 days
- **Complexity:** Medium
- **Dependencies:** None

## Related Issues
- TODO-001: Too Many Choices for Beginners
- TODO-002: No "Hello World" Example
- TODO-003: Prerequisites Check Could Be Automated
- TODO-017: NetworkPolicy Issues Cause Silent Failures
- TODO-018: Registry Errors Are Cryptic

## Success Metrics
- Authentication-related support requests: Reduce by 50%
- First-time success rate: Improve by 25%
- Time spent on credential setup: Reduce by 60%

## Notes
This is one of the top causes of user frustration. Clear, simple credential setup guidance with validation would significantly improve the onboarding experience.
