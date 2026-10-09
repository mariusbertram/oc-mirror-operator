# TODO-010: Cleanup Policy is Not Discoverable

## Metadata
- **Issue ID:** TODO-010
- **Title:** Cleanup Policy is Not Discoverable
- **Priority:** P1 (Important)
- **Severity:** Medium
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p1`, `category/configuration`

## Problem Description

The cleanup policy for removing orphaned images is not obvious:
1. Must set `mirror.openshift.io/cleanup-policy=Delete` annotation on ImageSet
2. No documentation in the main configuration guides
3. Behavior is not intuitive (doesn't clean up by default)
4. No clear explanation of what gets cleaned up

This leads to:
- Orphaned images accumulating in registry
- Wasted storage
- Confusion about cleanup behavior

## Impact
- **User Impact:** Medium - Users accidentally leave orphaned images
- **Business Impact:** Medium - Wasted storage costs
- **Frequency:** Medium - Affects users who modify or delete ImageSets

## Evidence

From `AGENTS.md` and `CLAUDE.md`:

```
### Annotation-Driven Flows
- `mirror.openshift.io/cleanup-policy=Delete`: enables image deletion from registry on ImageSet removal or spec narrowing
```

From controller code:

```go
// In ImageSetReconciler or cleanup logic
if imageSet.Annotations["mirror.openshift.io/cleanup-policy"] == "Delete" {
    // Enable cleanup
}
```

**Problems:**
- Only mentioned in developer documentation (AGENTS.md, CLAUDE.md)
- Not in user-facing documentation
- No explanation of what cleanup does
- No guidance on when to use it

## Recommended Solution

### 1. Add Cleanup Policy Documentation

Add to `docs/configuration/imagesets.md` and `docs/configuration/mirrortarget.md`:

```markdown
## Cleanup Policy

By default, when you remove an ImageSet or narrow its specification (e.g., remove a channel or package), the images that were mirrored for that ImageSet **remain in the target registry**. This prevents accidental data loss.

To automatically clean up orphaned images, enable the cleanup policy:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: my-imageset
  namespace: mirror
  annotations:
    mirror.openshift.io/cleanup-policy: "Delete"
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16
```

### What Gets Cleaned Up?

When cleanup is enabled:

| Action | Cleanup Behavior |
|--------|------------------|
| Remove ImageSet from MirrorTarget | Images exclusive to that ImageSet are deleted |
| Delete ImageSet | All images from that ImageSet are deleted |
| Narrow ImageSet spec (remove channel) | Images from removed channel are deleted |
| Narrow ImageSet spec (remove package) | Images from removed package are deleted |
| Update ImageSet (version range change) | Images outside new range are deleted |

**Important:** Only images that are **exclusively** referenced by the modified ImageSet are deleted. If an image is referenced by multiple ImageSets, it will NOT be deleted until the last reference is removed.

### When to Enable Cleanup

| Scenario | Enable Cleanup? | Notes |
|----------|-----------------|-------|
| Development/Testing | ✅ Yes | Frequent changes, want to save space |
| Production with strict retention | ❌ No | Manual cleanup process |
| Production with automated retention | ✅ Yes | Combined with other retention policies |
| Air-gap environments | ⚠️ Caution | Ensure you have backups before enabling |
| Multi-ImageSet mirrors | ✅ Yes | Automatically clean up orphaned images |

### Cleanup Examples

**Example 1: Enable cleanup for a development ImageSet**
```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: dev-imageset
  annotations:
    mirror.openshift.io/cleanup-policy: "Delete"
spec:
  mirror:
    additionalImages:
      - name: nginx:latest
      - name: alpine:latest
```

**Example 2: Disable cleanup for production (default)**
```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: prod-imageset
  # No cleanup-policy annotation = no cleanup
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16
```

**Example 3: Enable cleanup globally via MirrorTarget**
```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-target
  annotations:
    mirror.openshift.io/default-cleanup-policy: "Delete"
spec:
  registry: registry.example.com/mirror
  imageSets: [imageset-1, imageset-2]
```
```

### 2. Add Cleanup Policy to API

Add a field to ImageSet spec for cleaner configuration:

```go
// In api/v1alpha1/imageset_types.go
type ImageSetSpec struct {
    // ... existing fields ...
    
    // CleanupPolicy determines whether to delete images when they are no longer needed.
    // "Delete" - Automatically delete orphaned images from the target registry.
    // "Retain" (default) - Keep orphaned images in the target registry.
    // +optional
    CleanupPolicy CleanupPolicyType `json:"cleanupPolicy,omitempty"`
}

type CleanupPolicyType string

const (
    CleanupPolicyDelete CleanupPolicyType = "Delete"
    CleanupPolicyRetain CleanupPolicyType = "Retain"
)
```

This allows:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: my-imageset
spec:
  cleanupPolicy: Delete  # Cleaner than annotation
  mirror:
    platform:
      channels:
        - name: stable-4.16
```

**Migration:** Keep annotation support for backward compatibility, but prefer the spec field.

### 3. Add Cleanup Status and Logging

Add cleanup status to ImageSet and MirrorTarget:

```yaml
# In ImageSet status
status:
  conditions:
    - type: CleanupInProgress
      status: "True"
      reason: CleaningUp
      message: "Cleaning up 45 orphaned images from registry"
    - type: CleanupComplete
      status: "True"
      lastTransitionTime: "2024-01-15T10:30:00Z"
      message: "Successfully cleaned up 45 orphaned images"
  
  # In MirrorTarget status
  cleanup:
    enabled: true
    lastCleanupTime: "2024-01-15T10:30:00Z"
    imagesCleaned: 45
    imagesRemaining: 1234
```

Add cleanup logging:

```
# Manager log
INFO  Starting cleanup for ImageSet my-imageset
INFO  Found 45 orphaned images to clean up
INFO  Cleaning up image registry.example.com/mirror/openshift4/ose-cli:4.16.0
INFO  Cleaning up image registry.example.com/mirror/openshift4/ose-cli:4.16.1
...
INFO  Cleanup complete: 45 images deleted, 0 errors
```

### 4. Add Cleanup Dry-Run Mode

Add a way to see what would be cleaned up without actually deleting:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: my-imageset
  annotations:
    mirror.openshift.io/cleanup-policy: "Delete"
    mirror.openshift.io/cleanup-dry-run: "true"  # Preview only
```

Or via CLI:

```bash
# Preview cleanup for an ImageSet
mirrorctl cleanup preview my-imageset --namespace mirror

# Output:
ImageSet: my-imageset
Cleanup Policy: Delete

Images that would be cleaned up (45):
  registry.example.com/mirror/openshift4/ose-cli:4.16.0
  registry.example.com/mirror/openshift4/ose-cli:4.16.1
  registry.example.com/mirror/openshift4/ose-cli:4.16.2
  ...

Total size: 12.4 GB

To perform actual cleanup, remove the dry-run annotation or set cleanup-policy to Delete.
```

### 5. Add Cleanup CLI Commands

Add to mirrorctl:

```bash
# List cleanup status
mirrorctl cleanup status --namespace mirror

# Output:
IMAGESET          CLEANUP POLICY  LAST CLEANUP    IMAGES CLEANED  ORPHANED IMAGES
my-imageset-1     Delete          2h ago          45             0
my-imageset-2     Retain          N/A             0              12
my-imageset-3     Delete          1d ago          123            0

# Preview cleanup
mirrorctl cleanup preview my-imageset --namespace mirror

# Force cleanup (even if policy is Retain)
mirrorctl cleanup run my-imageset --namespace mirror --force

# View cleanup history
mirrorctl cleanup history --namespace mirror

# Output:
TIME                     IMAGESET      IMAGES CLEANED  STATUS
2024-01-15T10:30:00Z     my-imageset-1  45             Success
2024-01-15T09:15:00Z     my-imageset-2  12             Success
2024-01-14T14:20:00Z     my-imageset-3  0              Skipped (no orphans)
```

### 6. Add Cleanup Safety Features

Add safety checks to prevent accidental data loss:

```yaml
# In MirrorTarget spec
spec:
  cleanup:
    enabled: true
    # Safety settings
    dryRun: false           # Preview only, no actual deletion
    confirmRequired: true   # Require confirmation for deletion
    maxDeletionRate: 10     # Max images to delete per hour
    retentionPeriod: 7d     # Keep images for at least 7 days before deletion
```

Or via annotations:

```yaml
metadata:
  annotations:
    mirror.openshift.io/cleanup-confirm-required: "true"
    mirror.openshift.io/cleanup-max-rate: "10"
    mirror.openshift.io/cleanup-retention: "7d"
```

## Acceptance Criteria

✅ Cleanup policy clearly documented
✅ Cleanup behavior explained (what gets cleaned up)
✅ Guidance on when to enable cleanup
✅ Cleanup policy as spec field (not just annotation)
✅ Cleanup status and logging
✅ Dry-run mode for preview
✅ CLI commands for cleanup management
✅ Safety features to prevent accidental deletion

## Implementation Steps

1. **Add cleanup documentation** (1 day)
   - Document cleanup policy
   - Explain behavior
   - Add examples
   - Add guidance

2. **Add cleanupPolicy to ImageSet spec** (1 day)
   - Add field to API
   - Update CRD
   - Add validation
   - Maintain backward compatibility with annotation

3. **Add cleanup status** (1 day)
   - Add status fields
   - Update controller
   - Add logging

4. **Add dry-run mode** (0.5 day)
   - Add annotation support
   - Implement preview logic
   - Add CLI command

5. **Add CLI commands** (1 day)
   - `cleanup status`
   - `cleanup preview`
   - `cleanup run`
   - `cleanup history`

6. **Add safety features** (0.5 day)
   - Confirmation requirement
   - Rate limiting
   - Retention period

## Estimated Effort
- **Total:** 5-6 days
- **Complexity:** Medium
- **Dependencies:** API changes require CRD update

## Related Issues
- TODO-008: ImageSet-MirrorTarget Relationship is Confusing
- TODO-009: Concurrency Settings Need Guidance
- TODO-013: Recollect vs Force Resync is Confusing

## Success Metrics
- Accidental orphaned images: Reduce by 80%
- Cleanup-related support requests: Reduce by 70%
- Storage waste: Reduce by 60%

## Notes
This is an important improvement for users who want to manage their registry storage. The documentation updates alone (1 day) would provide significant value. Consider implementing the documentation first, then the API changes and CLI tools.
