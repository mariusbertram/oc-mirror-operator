# TODO-009: Concurrency Settings Need Guidance

## Metadata
- **Issue ID:** TODO-009
- **Title:** Concurrency Settings Need Guidance
- **Priority:** P1 (Important)
- **Severity:** Medium
- **Category:** Configuration Experience
- **Status:** Open
- **Labels:** `status/open`, `priority/p1`, `category/configuration`

## Problem Description

Users don't know what `concurrency` and `batchSize` values to use for their setup:
- No guidance on appropriate values for different registries
- No explanation of the trade-offs
- Warning about Quay but no presets for other registries
- No performance expectations

This leads to:
- Suboptimal performance (too low concurrency)
- Registry issues (too high concurrency for Quay)
- Confusion about batchSize vs concurrency

## Impact
- **User Impact:** Medium - Suboptimal performance or registry issues
- **Business Impact:** Medium - Wasted time and resources
- **Frequency:** Medium - Affects users tuning performance

## Evidence

From `docs/configuration/mirrortarget.md`:

```yaml
spec:
  concurrency: 1     # worker pods running at the same time (1–100, default 1)
  batchSize: 50      # images per worker pod (1–100, default 50)
```

```
- **Quay:** keep `concurrency: 1`. Quay's storage backend can corrupt concurrent uploads
  of the same blob into different repositories. Sequential workers still get most of the
  speed benefit because blobs already pushed by an earlier image are *mounted*
  (zero-copy) rather than re-uploaded, and the manager orders images so that shared
  layers come first.
- **Other registries:** raise `concurrency` until the source registry's rate limit or
  your bandwidth becomes the bottleneck. 3–5 is a good start.
```

**Problems:**
- Only mentions Quay specifically
- No presets for other common registries (Harbor, Nexus, Artifactory, etc.)
- No explanation of batchSize impact
- No performance guidance

## Recommended Solution

### 1. Add Registry-Specific Presets

Add to `docs/configuration/mirrortarget.md`:

```markdown
## Throughput Presets by Registry Type

| Registry | Concurrency | Batch Size | Notes |
|----------|-------------|------------|-------|
| **Quay** | 1 | 50 | Storage backend can corrupt concurrent uploads of same blob |
| **Harbor** | 3-5 | 20-30 | Good default for most setups |
| **Nexus** | 3-5 | 20-30 | Similar to Harbor |
| **Artifactory** | 3-5 | 20-30 | May need adjustment based on storage type |
| **registry:2** | 3-5 | 10-20 | Local registry, limited by disk I/O |
| **ECR** | 5-10 | 30-50 | AWS rate limits are generous |
| **ACR** | 5-10 | 30-50 | Azure rate limits are generous |
| **GCR** | 5-10 | 30-50 | Google rate limits are generous |
| **GitHub Container Registry** | 3-5 | 20-30 | Rate limited, start conservative |
| **GitLab Container Registry** | 3-5 | 20-30 | Rate limited, start conservative |

### Quick Start Presets

```yaml
# For Quay
concurrency: 1
batchSize: 50

# For Harbor/Nexus/Artifactory
concurrency: 3
batchSize: 20

# For Cloud Registries (ECR/ACR/GCR)
concurrency: 5
batchSize: 30

# For GitHub/GitLab
concurrency: 3
batchSize: 20

# For local registry:2
concurrency: 3
batchSize: 10
```
```

### 2. Add Performance Tuning Guide

Add to `docs/configuration/mirrortarget.md`:

```markdown
## Performance Tuning Guide

### Understanding the Settings

**`concurrency`**: Number of worker pods running simultaneously
- Each worker pod copies images in parallel
- Higher values = more parallelism, but more resource usage
- Limited by: registry rate limits, cluster resources, network bandwidth

**`batchSize`**: Number of images each worker pod copies
- Each worker gets a batch of images to copy
- Higher values = less pod overhead, but larger failure units
- If a worker fails, all images in its batch are retried

### Trade-offs

| Concurrency | Batch Size | Pros | Cons |
|-------------|------------|------|------|
| Low (1-2) | High (50-100) | Less registry load, fewer pods | Slower start, larger failure units |
| Medium (3-5) | Medium (20-30) | Good balance | May hit rate limits |
| High (5-10) | Low (10-20) | Maximum parallelism | High registry load, many pods |

### Performance Expectations

| Setup | Images/Hour | Notes |
|-------|-------------|-------|
| Quay, concurrency=1, batchSize=50 | 50-100 | Blob reuse helps significantly |
| Harbor, concurrency=3, batchSize=20 | 150-300 | Good for most setups |
| ECR, concurrency=5, batchSize=30 | 250-500 | Cloud registries handle load well |
| registry:2 (local), concurrency=3, batchSize=10 | 30-60 | Limited by disk I/O |

**Note:** Actual performance depends on:
- Image sizes (small images copy faster)
- Network latency and bandwidth
- Registry performance
- Cluster resources (CPU, memory, disk)

### Monitoring Performance

Check actual throughput:
```bash
# Images mirrored in last hour
kubectl get imageset <name> -n <ns> -o jsonpath='{.status.mirroredImages}'

# Time since last successful poll
kubectl get imageset <name> -n <ns> -o jsonpath='{.status.lastSuccessfulPollTime}'

# Worker pod completion time
kubectl get pods -n <ns> -l app=oc-mirror-worker -o jsonpath='{.items[*].status.containerStatuses[0].lastState.terminated.finishedAt}'
```

### Adjusting for Your Setup

**Start with presets**, then adjust based on observation:

1. **Too slow?** Increase `concurrency` first, then `batchSize`
2. **Registry errors?** Decrease `concurrency`
3. **Worker pods failing?** Decrease `batchSize` (smaller batches are more reliable)
4. **High resource usage?** Decrease both `concurrency` and `batchSize`
```

### 3. Add Auto-Tuning (Future Enhancement)

Consider adding auto-tuning in the future:

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-target
spec:
  registry: registry.example.com/mirror
  # Auto-tune based on registry type and performance
  autoTune: true
  maxConcurrency: 10  # Upper limit for auto-tuning
```

The operator could:
1. Detect registry type from the URL
2. Start with recommended presets
3. Monitor performance and adjust dynamically
4. Report tuning decisions in status

### 4. Add Configuration Generator with Presets

Update mirrorctl:

```bash
# Generate with registry-specific presets
mirrorctl generate mirrortarget my-target \
  --registry quay.example.com/mirror \
  --preset quay

# Output:
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: my-target
spec:
  registry: quay.example.com/mirror
  concurrency: 1  # Quay preset
  batchSize: 50   # Quay preset
  imageSets: []

# List available presets
mirrorctl presets list

# Output:
PRESET       CONCURRENCY  BATCHSIZE  DESCRIPTION
quay         1            50         Quay (corruption risk with higher concurrency)
harbor       3            20         Harbor
nexus        3            20         Nexus
ecr          5            30         AWS ECR
acr          5            30         Azure ACR
gcr          5            30         Google GCR
local        3            10         Local registry:2
github       3            20         GitHub Container Registry
gitlab       3            20         GitLab Container Registry
```

### 5. Add Performance Metrics

Add Prometheus metrics for performance monitoring:

```
# Throughput (images/hour)
mirror_operator_images_mirrored_total{imageset, mirrortarget}
mirror_operator_images_mirrored_duration_seconds{imageset, mirrortarget}

# Worker performance
mirror_operator_worker_images_per_batch{mirrortarget}
mirror_operator_worker_duration_seconds{mirrortarget}

# Registry performance
mirror_operator_registry_copy_duration_seconds{registry, imageset}
```

Add a performance dashboard:
```bash
# View performance metrics
mirrorctl metrics --namespace mirror

# Output:
MirrorTarget: internal-registry
  Current Throughput: 85 images/hour
  Average Batch Time: 4m 32s
  Images Mirrored (24h): 2040
  
Worker Performance:
  Average Images/Batch: 18.5
  Average Batch Time: 4m 32s
  Success Rate: 98.7%

Registry Performance:
  registry.redhat.io: Avg 2.1 MB/s
  registry.example.com: Avg 4.5 MB/s
```

## Acceptance Criteria

✅ Registry-specific presets documented
✅ Performance tuning guide with expectations
✅ Configuration generator with presets
✅ Performance metrics and monitoring
✅ Clear explanation of concurrency vs batchSize trade-offs

## Implementation Steps

1. **Add presets table to documentation** (0.5 day)
   - Research registry characteristics
   - Create presets table
   - Add recommendations

2. **Add performance tuning guide** (1 day)
   - Explain settings
   - Add trade-offs table
   - Add performance expectations
   - Add monitoring section

3. **Update configuration generator** (1 day)
   - Add preset support
   - Add `presets list` command
   - Generate with presets

4. **Add performance metrics** (2 days)
   - Add Prometheus metrics
   - Update dashboards
   - Add CLI metrics view

## Estimated Effort
- **Total:** 4-5 days
- **Complexity:** Medium
- **Dependencies:** None

## Related Issues
- TODO-005: YAML Configuration is Error-Prone
- TODO-010: Cleanup Policy is Not Discoverable
- TODO-029: No Performance Guidelines

## Success Metrics
- Optimal configuration selections: Increase by 60%
- Performance-related issues: Reduce by 50%
- Support requests for tuning: Reduce by 70%

## Notes
This is an important improvement for users who want to optimize their mirroring performance. The documentation updates alone (1-2 days) would provide significant value. Consider implementing the documentation first, then the CLI enhancements.
