# Configuring ImageSets

An `ImageSet` describes **what** to mirror. It has four content sections — OpenShift
releases, operator catalogs, Helm charts, additional images — plus a block list and a
signature requirement. This page walks through each section with examples; every field
is listed in the [API reference](../reference/api.md#imageset).

**Contents**

- [Skeleton](#skeleton)
- [OpenShift and OKD releases](#openshift-and-okd-releases)
- [Operator catalogs](#operator-catalogs)
- [Additional images](#additional-images)
- [Helm charts](#helm-charts)
- [Blocked images](#blocked-images)
- [Requiring signed images](#requiring-signed-images)
- [How many ImageSets?](#how-many-imagesets)
- [Editing from the console plugin](#editing-from-the-console-plugin)

---

## Skeleton

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-16
  namespace: mirror
spec:
  mirror:
    platform: { ... }          # OpenShift / OKD releases
    operators: [ ... ]         # OLM catalogs and packages
    helm: { ... }              # Helm chart images
    additionalImages: [ ... ]  # anything else
    blockedImages: [ ... ]     # exclusions applied to all of the above
    requireSignedImages: false
```

All sections are optional. An `ImageSet` only starts mirroring once a
[`MirrorTarget`](mirrortarget.md) lists it in `spec.imageSets`.

## OpenShift and OKD releases

Releases are resolved from the Cincinnati upgrade graph (`api.openshift.com`). For every
selected release the payload image plus all component images it references (~190) are
mirrored, tagged `<version>-<arch>[-<component>]` under `openshift/release-images` and
`openshift/release`.

### Selecting versions

```yaml
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"
          maxVersion: "4.16.30"
```

| `minVersion` | `maxVersion` | `shortestPath` | `full` | Result |
|---|---|---|---|---|
| — | — | — | — | Only the newest release in the channel |
| set | — | — | — | Every release ≥ `minVersion` (tracks new releases) |
| — | set | — | — | Exactly `maxVersion` |
| set | set | `false` | — | Every release in `[min, max]` |
| set | set | `true` | — | Only the releases on the shortest upgrade path from `min` to `max` |
| — | — | — | `true` | Every release in the channel |

Use `shortestPath: true` to prepare an upgrade from a known starting version: it mirrors
the intermediate releases the cluster will actually step through and nothing else.

Several channels can be listed; each is resolved independently.

### Architectures

```yaml
platform:
  architectures: [amd64, arm64]        # amd64 | arm64 | s390x | ppc64le
```

Every listed architecture is resolved separately: the Cincinnati graph is queried per
architecture, each release gets its own payload and component images tagged
`<version>-<arch name>` (`x86_64`, `aarch64`, `s390x`, `ppc64le`), and the version
selection above applies per architecture. KubeVirt container disks are extracted for all
listed architectures where the release provides them.

### OKD

```yaml
channels:
  - name: stable-4.16
    type: okd
    skipSignatureVerification: true
```

Release payloads are verified against the embedded Red Hat release signing keys before
they are mirrored; the signatures are fetched from `mirror.openshift.com`. OKD, CI and
nightly payloads are not signed with those keys, so their verification always fails and
the release would be skipped. Set `skipSignatureVerification: true` for such channels.

### KubeVirt container disks

```yaml
platform:
  kubeVirtContainer: true
```

Extracts the RHCOS KubeVirt container-disk image from each release payload (from the
`coreos-bootimages` ConfigMap embedded in the payload) and mirrors it as
`openshift/release:<version>-<arch>-kube-virt-container`. Architectures without a
KubeVirt image are skipped silently.

### OSUS graph-data image

```yaml
platform:
  graph: true
```

Downloads the current Cincinnati graph-data archive from `api.openshift.com`, packages it
into a UBI9-based image the same way oc-mirror v2 does, and pushes it to
`<registry>/openshift/graph-image:latest` for the OpenShift Update Service in the
disconnected cluster. The image is rebuilt at most once per `pollInterval`, or
immediately on a [recollect](../operations.md#recollect).

## Operator catalogs

Operator content is resolved from the catalog's file-based catalog (FBC): the operator
pulls the catalog image, filters the FBC to the packages you ask for (plus their
dependencies), and mirrors every bundle image and every related image those bundles
reference. Separately, a filtered **catalog image** is built and pushed so the
disconnected cluster gets a `CatalogSource`/`ClusterCatalog` that only advertises
what was mirrored.

### The heads-only default

```yaml
spec:
  mirror:
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal
          - name: openshift-pipelines-operator-rh
```

A package without further qualifiers mirrors **the newest bundle of every channel** —
the same behaviour as oc-mirror v2. That is what most users want: the cluster can
install and upgrade to the current version, and the mirror stays small.

### Selecting more than the head

| Goal | Configuration |
|---|---|
| Head plus N previous versions per channel | `previousVersions: 2` |
| Only some channels, all their versions | `channels: [{name: stable-4.16}]` |
| A version range within a channel | `channels: [{name: stable, minVersion: "1.2.0", maxVersion: "1.4.0"}]` |
| A version range across all channels | `minVersion: "1.2.0"` / `maxVersion: "1.4.0"` on the package |
| Change the default channel the catalog advertises | `defaultChannel: stable` |

```yaml
packages:
  - name: advanced-cluster-management
    channels:
      - name: release-2.11
  - name: quay-operator
    minVersion: "3.10.0"
    maxVersion: "3.12.0"
  - name: cert-manager
    previousVersions: 1
```

`previousVersions` only applies in heads-only mode, i.e. when the package has no
`channels`, `minVersion` or `maxVersion`.

### Whole catalog

```yaml
operators:
  - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
    full: true
```

### Dependencies

Dependencies are resolved transitively from the bundles' properties:
`olm.package.required`, `olm.gvk.required` (mapped to the providing package), and Red
Hat's companion packages (`<name>-dependencies`, `<name>-deps`). Dependency packages are
included in full. Disable this with `skipDependencies: true` when you manage
dependencies explicitly.

### Where the catalog image goes

By default the filtered catalog is pushed to the full source reference under the target
registry (`<registry>/registry.redhat.io/redhat/redhat-operator-index:v4.16`). Override
with:

```yaml
operators:
  - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
    targetCatalog: olm/redhat-operators      # path under the target registry
    targetTag: "4.16"
```

Changing `targetCatalog` or `targetTag` rebuilds and pushes the catalog to the new
reference; the old image stays in the registry.

### Catalog versions side by side

Two entries for `…-operator-index:v4.16` and `…-operator-index:v4.17` are kept apart:
each gets its own slug (`redhat-operator-index-v4.16`, `redhat-operator-index-v4.17`),
its own filtered image, and its own `CatalogSource`.

### Verifying the catalog signature

Third-party catalogs can be pinned to a cosign public key. A catalog whose signature does
not verify is skipped on that resolution pass (its previously mirrored state is kept) and
retried on the next poll.

```bash
kubectl create secret generic acme-catalog-key --from-file=cosign.pub=./acme.pub -n mirror
```

```yaml
operators:
  - catalog: registry.acme.example.com/acme/operator-index:v1
    signatureVerification:
      publicKeySecretRef:
        name: acme-catalog-key
        key: cosign.pub
    packages:
      - name: acme-operator
```

## Additional images

```yaml
spec:
  mirror:
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
      - name: quay.io/prometheus/prometheus:v2.53.0
        targetRepo: monitoring/prometheus       # optional path under the target registry
        targetTag: v2.53.0-mirror               # optional tag
      - name: quay.io/org/app@sha256:4f0e…        # digest references are supported
```

Without overrides the full source reference is kept (`<registry>/quay.io/prometheus/…`).
Tag-referenced images are re-checked against upstream during every drift check; if the
tag moved, the image is mirrored again.

## Helm charts

```yaml
spec:
  mirror:
    helm:
      repositories:
        - name: bitnami
          url: https://charts.bitnami.com/bitnami
          charts:
            - name: nginx
              version: "15.5.1"          # empty = latest non-prerelease
            - name: redis
              imagePaths:                 # extra JSONPath expressions
                - "{.spec.extraContainerImage}"
```

Each chart is downloaded from the repository index and **fully rendered** with the Helm
SDK (default values and capabilities, like `helm template`). Every rendered manifest is
then scanned for images at the default JSONPath locations
(`containers[*].image`, `initContainers[*].image`, in pod and pod-template specs) plus
any `imagePaths` you add. Images are mirrored under their full source reference.

Charts are re-rendered on every poll; there is no per-chart digest cache. Local charts
(`helm.local`) are **not** supported — the manager pod has no host filesystem.

## Blocked images

```yaml
spec:
  mirror:
    blockedImages:
      - name: openshift4/ose-jenkins-agent-base          # every tag and digest
      - name: redhat/postgresql-operator-bundle:v1        # only this tag
      - name: nvidia/driver@sha256:d4639…                 # only this digest
```

Blocked images are removed from the resolved set regardless of which section produced
them. Matching ignores the registry host. An image that was already mirrored becomes an
orphan and is deleted only if the `MirrorTarget` has
[`cleanup-policy: Delete`](mirrortarget.md#cleanup-policy).

## Requiring signed images

```yaml
spec:
  mirror:
    requireSignedImages: true
```

During the drift check, every mirrored image of the `ImageSet` must carry a well-formed
cosign signature at its destination (digest-bound; self-consistent with an embedded
certificate when present). Images without one are marked `Failed` with a descriptive
error and go through the normal retry cycle. This is a **presence check**, not trust
verification — use `operators[].signatureVerification` to pin a specific key, and note
that release component images are trusted through the GPG-verified payload rather than
individual signatures.

## How many ImageSets?

Split content along the lines you will change independently — typically one `ImageSet`
per OpenShift minor version for releases, one per catalog version for operators, and one
for miscellaneous images:

```yaml
kind: MirrorTarget
spec:
  imageSets:
    - ocp-4-16-releases
    - ocp-4-16-operators
    - tooling
```

Benefits: a change to one `ImageSet` only re-resolves that one; the catalog build gate
("no pending image left") applies per `ImageSet`, so a slow release mirror does not delay
an operator catalog; cleanup is scoped per `ImageSet`. Images referenced by several
`ImageSet`s are mirrored once and protected from cleanup until no `ImageSet` needs them.

## Editing from the console plugin

On OpenShift the [console plugin](../consuming-results.md#openshift-console-plugin)
edits most of this spec through the Resource API: catalogs and package filters
(**Operators**, **Catalogs** tabs), release channels (**Releases**), Helm repositories,
additional images, blocked images and `requireSignedImages`. `signatureVerification`
remains YAML-only.
