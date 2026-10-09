# TODO-001: Too Many Choices for Beginners

## Metadata
- **Issue ID:** TODO-001
- **Title:** Too Many Choices for Beginners
- **Priority:** P0 (Critical)
- **Severity:** High
- **Category:** Onboarding Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p0`, `category/onboarding`

## Problem Description

Users are presented with too many choices when starting with oc-mirror-operator:
- 4 different quick start guides (single operator, releases, helm, disconnected)
- 2 installation methods (OLM vs plain manifests)
- Multiple credential setup methods
- Various configuration options without clear guidance

This leads to **decision paralysis** where beginners don't know where to start.

## Impact
- **User Impact:** High - Beginners spend significant time trying to understand which path to take
- **Business Impact:** High - Reduces adoption rate and increases support burden
- **Frequency:** High - Affects every new user

## Evidence

From `docs/quickstart.md`:
```
| Use Case | Guide |
|---|---|
| Mirror a single operator | [Single Operator](quickstart-operator.md) |
| Mirror OpenShift releases | [OpenShift Releases](quickstart-release.md) |
| Mirror Helm charts | [Helm Charts](quickstart-helm.md) |
| Full disconnected cluster setup | [Disconnected Cluster](quickstart-disconnected.md) |
```

From `docs/quickstart-operator.md`, users must choose between:
- OLM (Recommended for OpenShift)
- Plain Manifests (Recommended for Kubernetes)

From `docs/quickstart-operator.md`, 3 different credential methods:
- Method 1: From existing Docker config
- Method 2: From OpenShift pull secret
- Method 3: Manual creation

## Recommended Solution

### 1. Create a Decision Tree in README
Add a clear decision tree at the top of the README:

```
## Quick Start Decision Guide

**What do you want to mirror?**
- Just trying it out? → [5-Minute Quick Start](#5-minute-quick-start) ⭐ NEW
- A single operator? → [Quick Start: Single Operator](docs/quickstart-operator.md)
- OpenShift releases? → [Quick Start: OpenShift Releases](docs/quickstart-release.md)
- Helm charts? → [Quick Start: Helm Charts](docs/quickstart-helm.md)
- Full disconnected cluster? → [Disconnected Cluster Setup](docs/quickstart-disconnected.md)

**Where are you running?**
- OpenShift? Use OLM installation
- Kubernetes? Use plain manifests
```

### 2. Create a "5-Minute Quick Start"
Create a new file `docs/quickstart-5minute.md` that:
- Mirrors a single public image (e.g., `nginx:latest`)
- Uses a local `registry:2` instance for simplicity
- Requires minimal prerequisites
- Shows immediate results
- Takes <5 minutes to complete

### 3. Simplify Quick Start Landing Page
Restructure `docs/quickstart.md` to:
- Start with the 5-minute quick start
- Then present scenario-based guides
- Add clear labels: "Beginner", "Intermediate", "Advanced"

### 4. Add Platform Detection
In the main README, add:
```bash
# Auto-detect platform and suggest installation method
if [ "$(kubectl config view -o jsonpath='{.clusters[0].server}' | grep -q openshift)" ]; then
  echo "OpenShift detected - use OLM installation"
else
  echo "Kubernetes detected - use plain manifests"
fi
```

## Acceptance Criteria

✅ A new user can start mirroring within 5 minutes using the 5-minute guide
✅ The main README clearly guides users to the right starting point
✅ Users don't feel overwhelmed by choices on first visit
✅ Each quick start guide clearly states its prerequisites and time estimate
✅ There's a clear progression path from beginner to advanced usage

## Implementation Steps

1. **Create `docs/quickstart-5minute.md`** (1 day)
   - Simple example with nginx:latest
   - Local registry setup instructions
   - Minimal configuration
   
2. **Update `README.md`** (0.5 day)
   - Add decision tree
   - Prominently feature the 5-minute guide
   - Reorganize quick start links

3. **Update `docs/quickstart.md`** (0.5 day)
   - Restructure to start with 5-minute guide
   - Add difficulty labels to guides
   - Improve navigation

4. **Add platform detection helper script** (0.5 day)
   - Create `hack/detect-platform.sh`
   - Document in README

## Estimated Effort
- **Total:** 2-3 days
- **Complexity:** Low
- **Dependencies:** None

## Related Issues
- TODO-002: No "Hello World" Example (this is essentially the same solution)
- TODO-004: Credential Setup is Confusing
- TODO-021: Documentation is Overwhelming for Beginners

## Success Metrics
- Time to first successful mirror: <15 minutes (currently ~30-60+ minutes)
- Percentage of users who start with the 5-minute guide: >50%
- First-time success rate: Improve by 20%

## Notes
This is one of the highest-impact, lowest-effort improvements. Addressing this will significantly improve the beginner experience and reduce support burden.
