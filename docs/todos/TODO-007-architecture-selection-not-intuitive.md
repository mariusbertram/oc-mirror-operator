# TODO-007: Architecture Selection is Not Intuitive

## Metadata
- **Issue ID:** TODO-007
- **Title:** Architecture Selection is Not Intuitive
- **Priority:** P2 (Nice-to-Have)
- **Severity:** Medium
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p2`, `category/configuration`

## Problem Description

Users don't understand:
1. Which architectures are available
2. Which architectures they need
3. What `multi` architecture means
4. How to specify multiple architectures
5. Which architectures are supported by which channels

This leads to:
- Wrong architecture selection
- Incomplete mirrors (missing architectures)
- Confusion about `multi` vs individual architectures

## Impact
- **User Impact:** Medium - Users may create incomplete mirrors
- **Business Impact:** Medium - Wasted storage and bandwidth
- **Frequency:** Medium - Affects users setting up multi-architecture clusters

## Evidence

From `api/v1alpha1/imageset_config_types.go`:

```go
// Architectures is a list of architectures to mirror (e.g. "amd64", "arm64", "s390x", "ppc64le", "multi")
// +optional
Architectures []string `json:"architectures,omitempty"`
```

From `docs/configuration/imagesets.md`:

```yaml
platform:
  architectures: [amd64]  # Can also include: arm64, s390x, ppc64le, multi
```

**Problems:**
- No explanation of what each architecture is
- No guidance on which to choose
- No explanation of `multi` architecture
- No information about architecture support by channel

## Recommended Solution

### 1. Add Architecture Documentation

Add to `docs/configuration/imagesets.md`:

```markdown
## Architecture Selection

### Available Architectures

| Architecture | Description | Common Use Cases |
|--------------|-------------|------------------|
| `amd64` | x86-64 (Intel/AMD 64-bit) | Most common, general purpose |
| `arm64` | ARM 64-bit | Cloud (AWS Graviton), Raspberry Pi, Apple Silicon |
| `s390x` | IBM Z (S390x) | IBM mainframes |
| `ppc64le` | PowerPC 64-bit Little Endian | IBM Power Systems |
| `multi` | Multi-architecture | Single image with manifests for multiple architectures |

### Which Architectures Should You Mirror?

| Cluster Type | Recommended Architectures |
|--------------|--------------------------|
| Single-architecture (x86) | `amd64` |
| Single-architecture (ARM) | `arm64` |
| Multi-architecture | `amd64, arm64` (or more) |
| All architectures | `amd64, arm64, s390x, ppc64le` |
| Using multi-arch images | `multi` (or include `multi` plus individual) |

### What is `multi` Architecture?

The `multi` architecture refers to **multi-architecture container images** that contain
manifests for multiple architectures in a single image reference. When you include `multi`:

- The operator will mirror the multi-arch manifest
- It will also mirror the individual architecture variants if they're referenced
- Use `multi` if you want to support clients that pull multi-arch images

**Example:** `registry.redhat.io/ubi8/ubi:latest` has a multi-arch manifest pointing to
`amd64`, `arm64`, `s390x`, and `ppc64le` variants.

### Architecture Support by Channel

| Channel | amd64 | arm64 | s390x | ppc64le | multi |
|---------|-------|-------|-------|--------|-------|
| stable-4.15 | ✅ | ✅ | ✅ | ✅ | ⚠️ |
| stable-4.16 | ✅ | ✅ | ✅ | ✅ | ⚠️ |
| stable-4.17 | ✅ | ✅ | ✅ | ✅ | ⚠️ |
| fast-4.16 | ✅ | ✅ | ❌ | ❌ | ⚠️ |
| eus-4.16 | ✅ | ✅ | ❌ | ❌ | ⚠️ |

✅ Fully supported
⚠️ Limited support (may not include all components)
❌ Not supported

**Note:** `multi` architecture support varies. Check your specific channel for details.
```

### 2. Add Architecture Validation

Add to `cmd/mirrorctl/main.go`:

```bash
# List supported architectures
mirrorctl architectures list

# Output:
ARCHITECTURE  DESCRIPTION
amd64         x86-64 (Intel/AMD 64-bit)
arm64         ARM 64-bit
s390x         IBM Z (S390x)
ppc64le       PowerPC 64-bit Little Endian
multi         Multi-architecture

# Check architecture support for channel
mirrorctl architectures check stable-4.16 --arch arm64
# ✅ Architecture 'arm64' is supported in channel 'stable-4.16'

mirrorctl architectures check fast-4.16 --arch s390x
# ❌ Architecture 's390x' is NOT supported in channel 'fast-4.16'
# Supported architectures: amd64, arm64

# Validate architecture list
mirrorctl validate architectures "amd64, arm64"
# ✅ Valid architectures: amd64, arm64

mirrorctl validate architectures "amd64, x86"
# ❌ Invalid architecture: 'x86' (use 'amd64' for x86-64)
```

### 3. Add Architecture Presets

Add common architecture presets:

```yaml
# Preset: x86 only (most common)
architectures: [amd64]

# Preset: x86 + ARM (common for cloud)
architectures: [amd64, arm64]

# Preset: All supported architectures
architectures: [amd64, arm64, s390x, ppc64le]

# Preset: All including multi
architectures: [amd64, arm64, s390x, ppc64le, multi]

# Preset: Multi only (for multi-arch images)
architectures: [multi]
```

### 4. Add Architecture Selection to Interactive Config

Update the interactive configuration builder:

```bash
mirrorctl config releases

# Output:
? Which channel do you want to mirror? stable-4.16
? Which architectures do you need?
  ❏ amd64 (x86-64 - Most common)
  ❏ arm64 (ARM 64-bit - Cloud, Apple Silicon)
  ❏ s390x (IBM Z - Mainframes)
  ❏ ppc64le (PowerPC - IBM Power Systems)
  ❏ multi (Multi-architecture images)
  ❏ All supported
```

### 5. Add Architecture Information to Channel Details

Update channel details command:

```bash
mirrorctl channels describe stable-4.16

# Output:
Channel: stable-4.16
Type: official (Red Hat)
Latest Version: 4.16.30

Supported Architectures:
  ✅ amd64
  ✅ arm64
  ✅ s390x
  ✅ ppc64le
  ⚠️  multi (limited)

Available Versions: 4.16.0 - 4.16.30 (31 versions)
```

## Acceptance Criteria

✅ Clear documentation on architecture selection
✅ Architecture validation via CLI
✅ Architecture presets for common scenarios
✅ Architecture support information by channel
✅ Interactive architecture selection

## Implementation Steps

1. **Add architecture documentation** (0.5 day)
   - Document each architecture
   - Add selection guidance
   - Explain `multi` architecture
   - Add channel support table

2. **Implement architecture validation** (1 day)
   - Add `architectures list` command
   - Add `architectures check` command
   - Add architecture validation

3. **Add architecture presets** (0.5 day)
   - Document common presets
   - Add to examples

4. **Update interactive config** (0.5 day)
   - Add architecture selection
   - Show descriptions

5. **Add architecture info to channel details** (0.5 day)
   - Update `channels describe` command
   - Show supported architectures

## Estimated Effort
- **Total:** 3-4 days
- **Complexity:** Low
- **Dependencies:** Channel data (same as TODO-006)

## Related Issues
- TODO-005: YAML Configuration is Error-Prone
- TODO-006: Channel and Version Selection is Complex
- TODO-008: ImageSet-MirrorTarget Relationship is Confusing

## Success Metrics
- Incorrect architecture selections: Reduce by 80%
- Time spent understanding architectures: Reduce by 60%
- Support requests for architecture issues: Reduce by 50%

## Notes
This is a relatively simple improvement that would eliminate much of the confusion around architecture selection. Consider implementing alongside TODO-006 (Channel and Version Selection) as they share similar infrastructure.
