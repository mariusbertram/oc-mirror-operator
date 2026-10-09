# TODO-005: YAML Configuration is Error-Prone

## Metadata
- **Issue ID:** TODO-005
- **Title:** YAML Configuration is Error-Prone
- **Priority:** P0 (Critical)
- **Severity:** High
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p0`, `category/configuration`

## Problem Description

Manual YAML editing for ImageSet and MirrorTarget resources leads to common errors:
- Syntax errors (missing colons, incorrect indentation)
- Wrong field names (e.g., `imageSets` vs `imagesets`)
- Invalid values (e.g., wrong channel names, invalid version formats)
- Missing required fields
- Incorrect nesting/structure

These errors cause:
- Silent failures (resource created but nothing happens)
- Cryptic error messages from Kubernetes API
- Wasted time debugging configuration issues

## Impact
- **User Impact:** High - Configuration errors are a top cause of failures
- **Business Impact:** High - Increases support burden and user frustration
- **Frequency:** Very High - Affects almost every user

## Evidence

Common errors observed:

```yaml
# Error 1: Wrong field name (should be imageSets, not imagesets)
spec:
  imagesets: [my-imageset]  # WRONG
  # Correct: imageSets: [my-imageset]

# Error 2: Invalid channel name
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16  # May not exist for all architectures
        # Should validate against known channels

# Error 3: Invalid version format
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16
          minVersion: "4.16"  # WRONG - should be "4.16.0"
          maxVersion: "4.16"  # WRONG - should be "4.16.30"

# Error 4: Missing required fields
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-target
spec:
  # Missing: registry (required)
  # Missing: imageSets (at least one required)

# Error 5: Incorrect nesting
spec:
  mirror:
    platform:
      architectures: amd64  # WRONG - should be [amd64]
      channels: [stable-4.16]  # WRONG nesting
```

## Recommended Solution

### 1. Create a Configuration Validator

Create `cmd/mirrorctl/main.go` with a `validate` subcommand:

```bash
# Validate a YAML file
mirrorctl validate my-imageset.yaml

# Output:
✅ my-imageset.yaml: Valid ImageSet configuration

# Or with errors:
❌ my-imageset.yaml: Invalid configuration
  - spec.mirror.platform.channels[0].name: "stable-4.16" is not a valid channel for architecture "amd64"
  - spec.mirror.platform.channels[0].minVersion: "4.16" should be in format "X.Y.Z" (e.g., "4.16.0")
  - spec.mirror.platform.architectures: should be array, got string "amd64"

# Validate from stdin
cat my-imageset.yaml | mirrorctl validate -

# Validate applied resources
mirrorctl validate --from-cluster --namespace mirror
```

### 2. Add Dry-Run Mode to Operator

Add a `--dry-run` flag to the operator that validates configuration without making changes:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: my-imageset
  annotations:
    mirror.openshift.io/dry-run: "true"  # Validate but don't start mirroring
```

Or via label:
```bash
kubectl label imageset my-imageset mirror.openshift.io/dry-run=true
```

### 3. Add Configuration Generator

Add to `cmd/mirrorctl/main.go`:

```bash
# Generate ImageSet for OpenShift releases
mirrorctl generate imageset ocp-releases \
  --channel stable-4.16 \
  --min-version 4.16.20 \
  --max-version 4.16.30 \
  --architectures amd64,arm64 \
  --output my-imageset.yaml

# Generate ImageSet for single operator
mirrorctl generate imageset operator \
  --catalog registry.redhat.io/redhat/redhat-operator-index:v4.16 \
  --package web-terminal \
  --output my-imageset.yaml

# Generate MirrorTarget
mirrorctl generate mirrortarget my-target \
  --registry registry.example.com/mirror \
  --auth-secret registry-creds \
  --imagesets my-imageset \
  --output my-mirrortarget.yaml

# Generate complete setup
mirrorctl generate setup \
  --name my-mirror \
  --namespace mirror \
  --channel stable-4.16 \
  --registry registry.example.com/mirror \
  --output-dir ./my-mirror-config/
```

### 4. Add YAML Schema and Validation

Add JSON Schema validation for CRDs:

```yaml
# In CRD definition
validation:
  openAPIV3Schema:
    type: object
    properties:
      spec:
        type: object
        properties:
          mirror:
            type: object
            properties:
              platform:
                type: object
                properties:
                  architectures:
                    type: array
                    items:
                      type: string
                      enum: [amd64, arm64, s390x, ppc64le, multi]
                  channels:
                    type: array
                    items:
                      type: object
                      properties:
                        name:
                          type: string
                          pattern: '^(stable|fast|eus|candidate|nightly)-[0-9]+\.[0-9]+$'
                        minVersion:
                          type: string
                          pattern: '^[0-9]+\.[0-9]+\.[0-9]+$'
```

### 5. Add Configuration Examples

Create a library of validated configuration examples:

```bash
# List available examples
mirrorctl examples list

# Show example
mirrorctl examples show ocp-releases-simple

# Apply example
mirrorctl examples apply ocp-releases-simple --namespace mirror
```

Examples to include:
- `ocp-releases-simple`: Mirror OpenShift releases for one channel
- `ocp-releases-multi-arch`: Mirror for multiple architectures
- `operator-single`: Mirror a single operator package
- `operator-multiple`: Mirror multiple operators
- `helm-charts`: Mirror Helm charts
- `additional-images`: Mirror arbitrary images
- `combined`: Combined OpenShift + operators + Helm

### 6. Improve Error Messages

When configuration is invalid, provide clear, actionable error messages:

```
# Instead of:
Error from server (BadRequest): error when creating "my-imageset.yaml": 
ImageSet.mirror.openshift.io "my-imageset" is invalid: 
spec.mirror.platform.channels[0].minVersion: Invalid value: "4.16": 
should match pattern "^[0-9]+\.[0-9]+\.[0-9]+$"

# Show:
❌ Configuration Error in ImageSet "my-imageset"

  Field: spec.mirror.platform.channels[0].minVersion
  Value: "4.16"
  Expected: Format "X.Y.Z" (e.g., "4.16.0", "4.16.20")

  Suggested fix:
    spec:
      mirror:
        platform:
          channels:
            - name: stable-4.16
              minVersion: "4.16.0"  # ✅ Correct format
              maxVersion: "4.16.30"

  See: https://docs.oc-mirror-operator.io/configuration/imagesets#version-format
```

## Acceptance Criteria

✅ Configuration validator catches common YAML errors
✅ Dry-run mode validates without making changes
✅ Configuration generator creates valid YAML
✅ JSON Schema validation for CRDs
✅ Library of validated configuration examples
✅ Clear, actionable error messages for configuration issues

## Implementation Steps

1. **Create `cmd/mirrorctl/main.go`** (3 days)
   - Implement `validate` subcommand
   - Implement `generate` subcommand
   - Implement `examples` subcommand
   - Add YAML parsing and validation

2. **Add JSON Schema validation** (1 day)
   - Update CRD definitions
   - Add OpenAPI v3 schema
   - Test validation

3. **Add dry-run mode to operator** (2 days)
   - Add annotation/label support
   - Implement validation-only mode
   - Report validation results in status

4. **Create configuration examples** (1 day)
   - Create 6-10 common examples
   - Validate all examples
   - Add to repository

5. **Improve error messages** (1 day)
   - Update controller error handling
   - Add user-friendly error formatting
   - Add links to documentation

## Estimated Effort
- **Total:** 8-10 days
- **Complexity:** Medium
- **Dependencies:** None

## Related Issues
- TODO-001: Too Many Choices for Beginners
- TODO-002: No "Hello World" Example
- TODO-004: Credential Setup is Confusing
- TODO-006: Channel and Version Selection is Complex
- TODO-007: Architecture Selection is Not Intuitive
- TODO-008: ImageSet-MirrorTarget Relationship is Confusing
- TODO-009: Concurrency Settings Need Guidance

## Success Metrics
- Configuration-related errors: Reduce by 70%
- Time spent debugging configuration: Reduce by 80%
- First-time success rate: Improve by 30%
- Support requests for configuration issues: Reduce by 60%

## Notes
This is a high-impact improvement. A configuration validator and generator would eliminate most of the common setup errors and significantly improve the user experience. Consider prioritizing the validator first (2-3 days), then the generator (3-4 days).
