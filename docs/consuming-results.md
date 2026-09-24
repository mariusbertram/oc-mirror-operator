# Consuming the mirror

Mirroring is only half the job: a disconnected cluster needs mirror rules, release
signatures and a catalog that points at the mirror. This page covers the three ways to
get them — the Resource API, the OpenShift console plugin, and `MirrorExport` for
transfers by other means.

**Contents**

- [What the operator generates](#what-the-operator-generates)
- [Resource API](#resource-api)
- [Applying the resources on the disconnected cluster](#applying-the-resources-on-the-disconnected-cluster)
- [OpenShift console plugin](#openshift-console-plugin)
- [MirrorExport: resolve without copying](#mirrorexport-resolve-without-copying)

---

## What the operator generates

For every `MirrorTarget` the manager keeps the ConfigMap `oc-mirror-<target>-resources`
up to date after each reconcile:

| Key | Resource | Content |
|---|---|---|
| `idms.yaml` | `ImageDigestMirrorSet` | One `source → mirror` repository rule per digest-referenced source repository (release components, operator images, digest-pinned additional images) |
| `itms.yaml` | `ImageTagMirrorSet` | Same for tag-referenced sources (tag-based additional and Helm images) |
| `catalogsource-<slug>.yaml` | `CatalogSource` | OLM v0 catalog pointing at the filtered catalog image; carries the `authSecret` name as `secrets` |
| `clustercatalog-<slug>.yaml` | `ClusterCatalog` | OLM v1 equivalent |
| `index.json` | — | List of the keys above |

Only `Mirrored` images contribute to IDMS/ITMS, so the rules never point at something
that is not there yet. Release payload signatures live in the `<target>-signatures`
ConfigMap; catalog package listings in `oc-mirror-<target>-<slug>-packages` and
`…-upstream-packages`.

## Resource API

The Resource API (`oc-mirror-resource-api` Deployment, port 8081) serves those
ConfigMaps over HTTP. How it is reachable is decided per `MirrorTarget` by
[`spec.expose`](configuration/mirrortarget.md#exposing-the-resource-api):

```bash
# OpenShift Route (default there)
URL=https://$(kubectl get route internal-registry-resources -n mirror -o jsonpath='{.spec.host}')

# Ingress
URL=https://$(kubectl get ingress internal-registry-resources -n mirror -o jsonpath='{.spec.rules[0].host}')

# Service only / plain Kubernetes: port-forward
kubectl port-forward svc/oc-mirror-resource-api 8081:8081 -n mirror &
URL=http://localhost:8081
```

```bash
curl -sk $URL/api/v1/targets | jq .                                   # overview
curl -sk $URL/api/v1/targets/internal-registry | jq '.imageSets, .catalogs'
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/idms.yaml
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/itms.yaml
curl -sk $URL/api/v1/targets/internal-registry/signatures.yaml
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/catalogs/redhat-operator-index-v4.16/catalogsource.yaml
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/catalogs/redhat-operator-index-v4.16/clustercatalog.yaml
curl -sk $URL/api/v1/targets/internal-registry/catalogs/redhat-operator-index-v4.16/packages.json | jq '.packages[].name'
```

The catalog `slug` is the repository base name plus tag (`redhat-operator-index-v4.16`);
the target detail endpoint lists every catalog with its slug. Read endpoints need no
authentication; the full list including the write endpoints used by the console plugin
is in the [REST API reference](reference/rest-api.md).

## Applying the resources on the disconnected cluster

1. **Mirror rules** — apply the IDMS and ITMS. OpenShift rolls the change out to every
   node's container runtime configuration (nodes reboot on older versions).
   ```bash
   curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/idms.yaml | oc apply -f -
   curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/itms.yaml | oc apply -f -
   ```
2. **Release signatures** — required for `oc adm upgrade` against the mirror. The
   endpoint returns ConfigMaps in the format the cluster-version-operator expects.
   ```bash
   curl -sk $URL/api/v1/targets/internal-registry/signatures.yaml | oc apply -f -
   ```
3. **Catalog** — apply the `CatalogSource` (OLM v0) or `ClusterCatalog` (OLM v1). The
   referenced image is the filtered catalog in the mirror; the `CatalogSource` names the
   pull secret given as `MirrorTarget.spec.authSecret`, create it in
   `openshift-marketplace` if the mirror needs authentication. Disable the default Red
   Hat sources with `oc patch operatorhub cluster --type merge -p '{"spec":{"disableAllDefaultSources":true}}'`.
4. **Upgrade graph** — if `platform.graph: true`, point an `UpdateService` at
   `<registry>/openshift/graph-image:latest` and set the cluster's upstream to it (see
   the OpenShift docs on the OpenShift Update Service).
5. **Registry trust** — make sure the disconnected cluster trusts the mirror registry's
   CA (`additionalTrustedCA` in the cluster image config).

The resources are regenerated whenever the mirror changes; re-apply after adding
content. They are stable in shape, so a scheduled `curl | oc apply` on the disconnected
side is a reasonable automation.

## OpenShift console plugin

On OpenShift the controller deploys a console plugin (`oc-mirror-plugin` Deployment,
`ConsolePlugin` CR `oc-mirror-operator`) automatically when the `ConsolePlugin` CRD
exists and `PLUGIN_IMAGE` is set. It appears as a navigation entry in the web console
and needs no Route: the console proxies to the plugin backend, which acts on the API
with **your** console token, so cluster RBAC decides what you may edit.

| Page | What you can do |
|---|---|
| **MirrorTargets** | Progress per target and ImageSet; **Settings** tab to edit registry, credentials, concurrency, batch size and intervals |
| **ImageSet detail** | Counters and conditions; per-tab editing of **Operators** (catalog list), **Catalogs** (package/channel/version filters via the catalog browser), **Releases**, **Helm**, **Additional Images**, **Blocked Images**; the `requireSignedImages` switch; **Recollect** and **Force Resync** buttons; download links for IDMS/ITMS/CatalogSource |
| **Catalog browser** | Upstream packages and channels on the left, your selection on the right; add whole packages or single channels, set `minVersion`/`maxVersion` per channel, save |
| **Failed images** | `Failed` and permanently failed images with the registry error, retry count and owning ImageSet |

The plugin is removed together with its resources when the operator is uninstalled
(finalizer `mirror.openshift.io/plugin-cleanup` on the `ConsolePlugin` CR).

## MirrorExport: resolve without copying

`MirrorExport` runs the same resolution as an `ImageSet` — releases, catalogs, Helm
charts, additional images, blocked images — but copies nothing. It renders the result
as artifacts you can feed to `oc image mirror`, `skopeo sync`, `oc-mirror` or your own
tooling, e.g. to build a transfer for a truly air-gapped site.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorExport
metadata:
  name: ocp-4-16-export
  namespace: mirror
spec:
  mirror:                                  # same schema as ImageSet.spec.mirror
    platform:
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"
          maxVersion: "4.16.25"
          shortestPath: true
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal
  source:                                  # optional: credentials/TLS for resolving
    authSecret: registry-creds
    # registry: registry.local:5000       # resolve against an already-populated mirror
    # insecure: true
    # caBundle: { configMapName: corporate-ca }
  destination:
    registry: registry.airgap.example.com/mirror   # only used to compute references
    insecure: false
```

The controller creates an `export-build` Job (ServiceAccount `<name>-export`, allowed
to write exactly one ConfigMap). The Job resolves the content upstream — or against
`spec.source.registry`, which is treated as insecure/authenticated per the other
`source` fields; catalog references in `spec.mirror` must then point at that registry —
and writes the ConfigMap `<name>-artifacts`:

| Key | Content |
|---|---|
| `manifest.json` | `{"images": [{"source", "destination", "origin", "bundleRef"}]}` — every image to copy, with its destination under `destination.registry` |
| `buildspec.json` | Catalogs to build (`sourceCatalog`, `targetRef`, `packages`/`full`) and whether a graph-data image is wanted — content that must be **built**, not copied |
| `idms.yaml`, `itms.yaml` | Mirror rules for the destination |
| `catalogsource-<slug>.yaml`, `clustercatalog-<slug>.yaml` | Catalog resources for the destination |

```bash
kubectl get mirrorexport ocp-4-16-export -n mirror         # Ready=True/Rendered, totalImages
kubectl get cm ocp-4-16-export-artifacts -n mirror -o jsonpath='{.data.manifest\.json}' \
  | jq -r '.images[] | "\(.source) \(.destination)"' > copy-list.txt

# e.g. with skopeo
while read src dst; do skopeo copy --all docker://$src docker://$dst; done < copy-list.txt
```

A spec change re-runs the Job and replaces the artifacts. Building the filtered catalog
image and the graph-data image from `buildspec.json`, and the copy itself, are outside
the operator's scope — `catalog-builder` and `oc-mirror` can do both.

`MirrorExport` status: `Ready=False/ExportBuildRunning` while the Job runs,
`Ready=True/Rendered` when the artifacts are written, `Ready=False/ExportBuildFailed`
with the Job's log holding the reason.
