# TODO-006: Channel and Version Selection is Complex

## Metadata
- **Issue ID:** TODO-006
- **Title:** Channel and Version Selection is Complex
- **Priority:** P2 (Nice-to-Have)
- **Severity:** Medium
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p2`, `category/configuration`

## Problem Description

Understanding and selecting the correct channel and version parameters requires deep knowledge of:
- Cincinnati upgrade graph
- OpenShift release versioning scheme
- `minVersion` vs `maxVersion` behavior
- `shortestPath` vs `full` channel resolution
- Version ranges and upgrade paths

The current documentation explains these concepts but doesn't make it easy for users to select the right values.

## Impact
- **User Impact:** Medium - Users select wrong versions or don't understand what they're getting
- **Business Impact:** Medium - Can lead to incomplete mirrors or wasted storage
- **Frequency:** Medium - Affects users setting up OpenShift release mirroring

## Evidence

From `docs/configuration/imagesets.md`:

```
| `minVersion` | `maxVersion` | `shortestPath` | `full` | Result |
|---|---|---|---|---|
| — | — | — | — | Only the newest release in the channel |
| set | — | — | — | Every release ≥ `minVersion` (tracks new releases) |
| — | set | — | — | Exactly `maxVersion` |
| set | set | `false` | — | Every release in `[min, max]` |
| set | set | `true` | — | Only the releases on the shortest upgrade path from `min` to `max` |
| — | — | — | `true` | Every release in the channel |
```

From `api/v1alpha1/imageset_config_types.go`:

```go
type Channel struct {
    // Name is the name of the channel (e.g. "stable-4.16", "fast-4.16", "eus-4.16", "candidate-4.16")
    Name string `json:"name"`
    
    // Type can be "official" (default, Red Hat) or "okd" (OKD)
    // +optional
    Type ChannelType `json:"type,omitempty"`
    
    // MinVersion is the minimum version to include from this channel.
    // If not specified, all versions in the channel are included.
    // +optional
    MinVersion string `json:"minVersion,omitempty"`
    
    // MaxVersion is the maximum version to include from this channel.
    // If not specified, mirroring will include all versions up to the current head.
    // +optional
    MaxVersion string `json:"maxVersion,omitempty"`
    
    // ShortestPath indicates whether to only include versions on the shortest
    // upgrade path between MinVersion and MaxVersion.
    // +optional
    ShortestPath bool `json:"shortestPath,omitempty"`
    
    // Full indicates whether to include all versions in the channel.
    // When true, MinVersion and MaxVersion are ignored.
    // +optional
    Full bool `json:"full,omitempty"`
}
```

## Recommended Solution

### 1. Add Channel and Version Presets

Create common presets for different use cases:

```yaml
# Preset: Latest stable only
mirror:
  platform:
    channels:
      - name: stable-4.16
        # Implicit: no min/max, no shortestPath = latest only

# Preset: Specific version only
mirror:
  platform:
    channels:
      - name: stable-4.16
        minVersion: "4.16.20"
        maxVersion: "4.16.20"

# Preset: Range of versions (all)
mirror:
  platform:
    channels:
      - name: stable-4.16
        minVersion: "4.16.0"
        maxVersion: "4.16.30"

# Preset: Upgrade path from A to B
mirror:
  platform:
    channels:
      - name: stable-4.16
        minVersion: "4.16.0"
        maxVersion: "4.16.30"
        shortestPath: true

# Preset: All versions in channel
mirror:
  platform:
    channels:
      - name: stable-4.16
        full: true
```

Add to documentation:

```markdown
## Channel Configuration Presets

### I want to mirror...

| Goal | Configuration | Notes |
|------|---------------|-------|
| The latest version | No min/max, no shortestPath | Gets newest, tracks new releases |
| A specific version | minVersion = maxVersion = "X.Y.Z" | Exactly one version |
| All versions in a range | minVersion, maxVersion, shortestPath: false | All versions between |
| Upgrade path | minVersion, maxVersion, shortestPath: true | Only versions on upgrade path |
| Everything in channel | full: true | All versions, tracks new releases |

### Common Scenarios

**Scenario: Prepare for new cluster installation**
```yaml
channels:
  - name: stable-4.16
    minVersion: "4.16.30"  # Latest stable
    maxVersion: "4.16.30"
```

**Scenario: Prepare for upgrade from 4.16.10 to 4.16.30**
```yaml
channels:
  - name: stable-4.16
    minVersion: "4.16.10"
    maxVersion: "4.16.30"
    shortestPath: true  # Only versions you'll step through
```

**Scenario: Mirror entire channel for testing**
```yaml
channels:
  - name: stable-4.16
    full: true  # All versions, will grow over time
```
```

### 2. Add Channel Information Lookup

Add to `cmd/mirrorctl/main.go`:

```bash
# List available channels
mirrorctl channels list

# Output:
CHANNEL          TYPE      ARCHITECTURES    LATEST       SUPPORTED
stable-4.15      official  amd64,arm64,s390x,ppc64le  4.15.45  ✅ Yes
stable-4.16      official  amd64,arm64,s390x,ppc64le  4.16.30  ✅ Yes
stable-4.17      official  amd64,arm64,s390x,ppc64le  4.17.15  ✅ Yes
fast-4.16        official  amd64,arm64      4.16.31  ✅ Yes
eus-4.16         official  amd64,arm64      4.16.30  ✅ Yes
candidate-4.16   official  amd64           4.16.31  ✅ Yes

# Get channel details
mirrorctl channels describe stable-4.16

# Output:
Channel: stable-4.16
Type: official (Red Hat)
Architectures: amd64, arm64, s390x, ppc64le, multi
Latest Version: 4.16.30
Available Versions: 4.16.0, 4.16.1, ..., 4.16.30 (31 versions)
EUS Versions: 4.16.0, 4.16.15, 4.16.30

# Get versions in channel
mirrorctl channels versions stable-4.16

# Output:
4.16.0
4.16.1
4.16.2
...
4.16.30

# Get upgrade path between versions
mirrorctl channels path stable-4.16 --from 4.16.10 --to 4.16.30

# Output:
Upgrade Path from 4.16.10 to 4.16.30:
4.16.10 → 4.16.11 → 4.16.12 → 4.16.15 → 4.16.20 → 4.16.25 → 4.16.30
(7 versions, shortest path)

All versions in range: 21 versions
```

### 3. Add Version Validation

Add validation for version format and existence:

```bash
# Validate version format
mirrorctl validate version "4.16"
# Error: Version "4.16" is invalid. Use format "X.Y.Z" (e.g., "4.16.0")

mirrorctl validate version "4.16.30"
# ✅ Valid version format

# Check if version exists in channel
mirrorctl validate version "4.16.30" --channel stable-4.16
# ✅ Version 4.16.30 exists in channel stable-4.16

mirrorctl validate version "4.16.99" --channel stable-4.16
# ❌ Version 4.16.99 does not exist in channel stable-4.16
# Available versions: 4.16.0 - 4.16.30
```

### 4. Add Interactive Configuration Builder

Add to `cmd/mirrorctl/main.go`:

```bash
# Interactive configuration for OpenShift releases
mirrorctl config releases

# Output:
? Which channel do you want to mirror? [stable-4.15/stable-4.16/stable-4.17/fast-4.16]
? What's your starting version? [latest/specific version]
? What's your ending version? [latest/specific version/none]
? Do you want all versions or just the upgrade path? [all/upgrade path]
? Which architectures? [amd64/arm64/s390x/ppc64le/multi/all]

# Generated configuration:
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-16-releases
spec:
  mirror:
    platform:
      architectures: [amd64, arm64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.10"
          maxVersion: "4.16.30"
          shortestPath: true
```

### 5. Add Visual Channel Graph

Add to documentation or CLI:

```
Channel: stable-4.16

4.16.0 —— 4.16.1 —— 4.16.2 —— ... —— 4.16.30
         \                              /
          \                            /
           4.16.11 —— 4.16.12 —— 4.16.15
                                    \
                                     4.16.16

Shortest path from 4.16.10 to 4.16.30 (bold):
4.16.10 —— 4.16.11 —— 4.16.15 —— 4.16.20 —— 4.16.25 —— 4.16.30
```

## Acceptance Criteria

✅ Clear presets for common channel configuration patterns
✅ Channel information lookup via CLI
✅ Version validation (format and existence)
✅ Interactive configuration builder
✅ Improved documentation with examples for each use case

## Implementation Steps

1. **Add channel presets to documentation** (0.5 day)
   - Create presets table
   - Add common scenarios
   - Update existing examples

2. **Implement channel lookup in mirrorctl** (2 days)
   - Query Cincinnati API or cached data
   - Implement `channels list` command
   - Implement `channels describe` command
   - Implement `channels versions` command
   - Implement `channels path` command

3. **Add version validation** (1 day)
   - Version format validation
   - Version existence check
   - Channel membership check

4. **Add interactive configuration builder** (2 days)
   - Implement interactive prompts
   - Generate valid YAML
   - Validate inputs

5. **Add visual channel graph** (1 day)
   - Generate ASCII graph
   - Highlight upgrade paths
   - Add to documentation

## Estimated Effort
- **Total:** 6-7 days
- **Complexity:** Medium
- **Dependencies:** Access to Cincinnati API or cached channel data

## Related Issues
- TODO-005: YAML Configuration is Error-Prone
- TODO-007: Architecture Selection is Not Intuitive
- TODO-008: ImageSet-MirrorTarget Relationship is Confusing

## Success Metrics
- Time spent understanding channel configuration: Reduce by 50%
- Incorrect version selections: Reduce by 70%
- Support requests for channel/version issues: Reduce by 40%

## Notes
This improvement would make channel and version selection much more user-friendly. However, it has lower priority than the critical P0 issues. Consider implementing the documentation presets first (0.5 day) as a quick win, then the CLI tools (5-6 days).
