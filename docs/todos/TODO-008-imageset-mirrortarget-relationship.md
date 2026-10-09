# TODO-008: ImageSet-MirrorTarget Relationship is Confusing

## Metadata
- **Issue ID:** TODO-008
- **Title:** ImageSet-MirrorTarget Relationship is Confusing
- **Priority:** P1 (Important)
- **Severity:** Medium
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p1`, `category/configuration`

## Problem Description

The relationship between ImageSet and MirrorTarget is not intuitive:

1. **ImageSet does NOT reference MirrorTarget** - It's a one-way relationship
2. **MirrorTarget DOES reference ImageSet** - Via `spec.imageSets`
3. **An ImageSet can only be referenced by ONE MirrorTarget** - This is a hard constraint
4. **Both resources must be in the same namespace** - Operator is namespace-scoped

When users violate constraint #3, they get a cryptic error:
- ImageSet condition: `Ready=False` with reason `Unbound`
- Message: "ImageSet is referenced by multiple MirrorTargets" or "ImageSet is not referenced by any MirrorTarget"

This leads to:
- Confusion about how to associate ImageSets with MirrorTargets
- Frustration when getting "Unbound" errors
- Difficulty understanding the constraint

## Impact
- **User Impact:** Medium - Users create invalid configurations
- **Business Impact:** Medium - Increases support requests
- **Frequency:** Medium - Affects users creating multiple MirrorTargets

## Evidence

From `docs/concepts.md`:

```
Three invariants:

- The **`MirrorTarget` owns the association**. An `ImageSet` never references a target.
- An `ImageSet` may be referenced by **exactly one** `MirrorTarget`. A second reference
  puts the `ImageSet` into `Ready=False/Unbound` with an explanatory message.
- All three resources and the secrets/ConfigMaps they use live in the **same namespace**
  — the operator's own namespace, since the operator is namespace-scoped.
```

From `api/v1alpha1/imageset_types.go` and controller code:

```go
// In ImageSetReconciler
if len(imageSetReferences) > 1 {
    // ImageSet is referenced by multiple MirrorTargets
    setCondition(&imageSet.Status.Conditions, Condition{
        Type:   ImageSetReady,
        Status: metav1.ConditionFalse,
        Reason: "Unbound",
        Message: fmt.Sprintf("ImageSet is referenced by %d MirrorTargets, but can only be referenced by one", len(imageSetReferences)),
    })
    return nil
}
```

**Problems:**
- The constraint is documented but easy to miss
- Error message doesn't explain how to fix the issue
- No guidance on how to properly associate resources

## Recommended Solution

### 1. Improve Error Messages

Update the controller to provide clearer, actionable error messages:

```go
// Current error message:
Message: "ImageSet is referenced by 2 MirrorTargets, but can only be referenced by one"

// Improved error message:
Message: fmt.Sprintf(
    "ImageSet '%s' is referenced by %d MirrorTargets: %s. "+
    "An ImageSet can only be referenced by one MirrorTarget. "+
    "To fix: remove the ImageSet reference from all but one MirrorTarget.",
    imageSet.Name,
    len(imageSetReferences),
    strings.Join(imageSetReferences, ", "),
)
```

For the case where ImageSet is not referenced:

```go
// Current error message:
Message: "ImageSet is not referenced by any MirrorTarget"

// Improved error message:
Message: fmt.Sprintf(
    "ImageSet '%s' is not referenced by any MirrorTarget. "+
    "An ImageSet only takes effect when referenced by a MirrorTarget. "+
    "To fix: add this ImageSet to a MirrorTarget's spec.imageSets list.",
    imageSet.Name,
)
```

### 2. Add Relationship Documentation

Add to `docs/concepts.md` and `docs/configuration/imagesets.md`:

```markdown
## How ImageSet and MirrorTarget Work Together

```
                   ┌─────────────────┐
                   │   MirrorTarget   │
                   │  (WHERE + HOW)   │
                   │                 │
                   │  spec.imageSets  │──────┐
                   │    [set1]       │      │
                   └────────┬────────┘      │
                            │               │
                            ▼               ▼
                   ┌─────────────────┐    ┌─────────────────┐
                   │   ImageSet 1    │    │   ImageSet 2    │
                   │   (WHAT)        │    │   (WHAT)        │
                   │                 │    │                 │
                   │  spec.mirror    │    │  spec.mirror    │
                   │   (content)     │    │   (content)     │
                   └─────────────────┘    └─────────────────┘

### Key Rules:

1. **One-Way Relationship**: MirrorTarget → ImageSet (not the other way)
   - MirrorTarget references ImageSet via `spec.imageSets: [name1, name2, ...]`
   - ImageSet does NOT reference MirrorTarget

2. **One-to-One Constraint**: Each ImageSet can be referenced by **exactly ONE** MirrorTarget
   - ❌ WRONG: Two MirrorTargets referencing the same ImageSet
   - ✅ CORRECT: One MirrorTarget referencing the ImageSet

3. **Namespace Constraint**: Both resources must be in the **same namespace**
   - The operator is namespace-scoped
   - All resources must be in the operator's namespace

### Example: Correct Configuration

```yaml
# ImageSet defines WHAT to mirror (no reference to target)
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-16-releases
  namespace: mirror
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.16

# MirrorTarget defines WHERE and HOW (references the ImageSet)
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
  namespace: mirror
spec:
  registry: registry.example.com/mirror
  authSecret: registry-creds
  imageSets: [ocp-4-16-releases]  # ← References the ImageSet
```

### Example: Incorrect Configuration (Multiple References)

```yaml
# ❌ WRONG: Same ImageSet referenced by two MirrorTargets

# MirrorTarget 1
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: target-1
  namespace: mirror
spec:
  registry: registry1.example.com/mirror
  imageSets: [ocp-4-16-releases]  # ← First reference

# MirrorTarget 2
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: target-2
  namespace: mirror
spec:
  registry: registry2.example.com/mirror
  imageSets: [ocp-4-16-releases]  # ← Second reference (CAUSES ERROR)

# Result: ImageSet 'ocp-4-16-releases' will have Ready=False, Reason=Unbound
```

### How to Fix "Unbound" Errors

**Symptom:** ImageSet has condition `Ready=False` with reason `Unbound`

**Cause 1:** ImageSet not referenced by any MirrorTarget

**Fix:** Add the ImageSet to a MirrorTarget's `spec.imageSets` list.

**Cause 2:** ImageSet referenced by multiple MirrorTargets

**Fix:** Remove the ImageSet from all but one MirrorTarget's `spec.imageSets` list.
```
```

### 3. Add Validation in mirrorctl

Add to `cmd/mirrorctl/main.go`:

```bash
# Check ImageSet references
mirrorctl check imageset my-imageset --namespace mirror

# Output:
✅ ImageSet 'my-imageset' is referenced by 1 MirrorTarget: 'my-target'

# Or with error:
❌ ImageSet 'my-imageset' is referenced by 2 MirrorTargets: 'target-1', 'target-2'
   An ImageSet can only be referenced by one MirrorTarget.
   To fix: kubectl edit mirrortarget target-1 -n mirror
          Remove 'my-imageset' from spec.imageSets

# Check all ImageSets in namespace
mirrorctl check imagesets --namespace mirror

# Output:
IMAGESET          REFERENCED BY    STATUS
my-imageset-1     target-1         ✅ OK (1 reference)
my-imageset-2     target-1, target-2 ❌ ERROR (2 references)
my-imageset-3     (none)           ⚠️  WARNING (0 references)
```

### 4. Add Relationship Visualization

Add to mirrorctl:

```bash
# Show relationship graph
mirrorctl graph --namespace mirror

# Output:
Namespace: mirror

MirrorTargets:
  internal-registry
    └── ImageSets:
        ├── ocp-4-16-releases
        ├── ocp-4-16-operators
        └── additional-images

  backup-registry
    └── ImageSets:
        └── ocp-4-15-releases

Unreferenced ImageSets:
  test-imageset (not referenced by any MirrorTarget)

Orphaned references:
  old-target references: deleted-imageset (ImageSet doesn't exist)
```

### 5. Add Configuration Generator with Validation

Update the configuration generator to check for relationship issues:

```bash
# Generate and validate configuration
mirrorctl generate mirrortarget my-target \
  --registry registry.example.com/mirror \
  --imagesets my-imageset,shared-imageset

# If shared-imageset is already referenced:
❌ Error: ImageSet 'shared-imageset' is already referenced by MirrorTarget 'existing-target'
   An ImageSet can only be referenced by one MirrorTarget.
   Choose a different ImageSet or remove the reference from 'existing-target' first.
```

## Acceptance Criteria

✅ Clear error messages that explain the constraint and how to fix
✅ Improved documentation with examples of correct/incorrect configurations
✅ CLI tool to check ImageSet references
✅ Relationship visualization
✅ Configuration generator validates relationship constraints

## Implementation Steps

1. **Improve error messages in controller** (0.5 day)
   - Update Unbound error messages
   - Add actionable guidance
   - Include list of referencing MirrorTargets

2. **Update documentation** (1 day)
   - Add relationship explanation with diagrams
   - Add examples of correct/incorrect configurations
   - Add troubleshooting guide for "Unbound" errors

3. **Implement reference checking in mirrorctl** (1 day)
   - Add `check imageset` command
   - Add `check imagesets` command
   - Add clear output formatting

4. **Add relationship visualization** (1 day)
   - Implement `graph` command
   - Show MirrorTarget → ImageSet relationships
   - Highlight unreferenced ImageSets

5. **Update configuration generator** (0.5 day)
   - Add validation for ImageSet references
   - Check for existing references
   - Provide clear error messages

## Estimated Effort
- **Total:** 4-5 days
- **Complexity:** Medium
- **Dependencies:** None

## Related Issues
- TODO-005: YAML Configuration is Error-Prone
- TODO-010: Cleanup Policy is Not Discoverable
- TODO-013: Recollect vs Force Resync is Confusing

## Success Metrics
- "Unbound" error support requests: Reduce by 80%
- Time spent debugging relationship issues: Reduce by 70%
- Incorrect configurations: Reduce by 60%

## Notes
This is an important improvement that would eliminate much of the confusion around the ImageSet-MirrorTarget relationship. The improved error messages alone would be a significant help. Consider implementing the error message improvements first (0.5 day) as a quick win.
