# REST API reference

Two HTTP servers expose the same code (`pkg/resourceapi`):

| Server | Where | Port | Auth | Serves |
|---|---|---|---|---|
| **Resource API** | Deployment `oc-mirror-resource-api`, one per namespace; reached through the `<target>-resources` Service and `spec.expose` | 8081 | Read endpoints anonymous (served with the Resource API's read-only service account); write endpoints **require** the caller's bearer token (`Authorization: Bearer …` or `X-Forwarded-Access-Token`), act with that user's RBAC, and answer `401` without one | JSON + YAML endpoints below |
| **Console plugin backend** | Deployment `oc-mirror-plugin` (OpenShift) | 9443 (HTTPS, service serving cert) | Console forwards the logged-in user's token | Same endpoints plus the plugin's static assets |

All paths are relative to the base URL, e.g.
`https://$(kubectl get route <target>-resources -o jsonpath='{.spec.host}')`.

## Read endpoints

| Method | Path | Returns |
|---|---|---|
| GET | `/api/v1/targets` | All MirrorTargets in the namespace with counters and ImageSets |
| GET | `/api/v1/targets/{mt}` | One MirrorTarget: counters, conditions, ImageSets with resource links, catalogs with slugs |
| GET | `/api/v1/targets/{mt}/image-failures` | Failed and pending images across the target's ImageSets, with error, retry count and owning ImageSet |
| GET | `/api/v1/targets/{mt}/imagesets/{is}/idms.yaml` | `ImageDigestMirrorSet` for the target (the `{is}` segment is kept for compatibility; the document covers the whole target) |
| GET | `/api/v1/targets/{mt}/imagesets/{is}/itms.yaml` | `ImageTagMirrorSet` for the target |
| GET | `/api/v1/targets/{mt}/imagesets/{is}/catalogs/{slug}/catalogsource.yaml` | OLM v0 `CatalogSource` for the filtered catalog |
| GET | `/api/v1/targets/{mt}/imagesets/{is}/catalogs/{slug}/clustercatalog.yaml` | OLM v1 `ClusterCatalog` |
| GET | `/api/v1/targets/{mt}/catalogs/{slug}/packages.json` | Packages, channels and bundle versions of the **filtered** catalog |
| GET | `/api/v1/targets/{mt}/catalogs/{slug}/upstream-packages.json` | Packages, channels and channel heads of the **upstream** catalog |
| GET | `/api/v1/targets/{mt}/signatures.yaml` | ConfigMaps with the verified GPG signatures of all mirrored release payloads (OpenShift signature-store format) |
| GET | `/api/v1/targets/{namespace}/{name}/spec` | Editable subset of the MirrorTarget spec |
| GET | `/api/v1/imagesets/{namespace}/{name}/operators` | Catalog entries (`catalog`, `targetCatalog`, `targetTag`, `full`, `skipDependencies`) |
| GET | `/api/v1/imagesets/{namespace}/{name}/catalogs/{slug}/packages` | Package filters of one catalog entry |
| GET | `/api/v1/imagesets/{namespace}/{name}/releases` | `platform` section |
| GET | `/api/v1/imagesets/{namespace}/{name}/helm` | `helm` section |
| GET | `/api/v1/imagesets/{namespace}/{name}/additional-images` | `additionalImages` |
| GET | `/api/v1/imagesets/{namespace}/{name}/blocked-images` | `blockedImages` |
| GET | `/api/v1/imagesets/{namespace}/{name}/settings` | `requireSignedImages` |
| GET | `/api/v1/releases/channels` | Available OCP channels for the release editor (from GitHub, ConfigMap or built-in fallback) |
| GET | `/resources/{is}/…` | Legacy paths, redirected to `/api/v1/…` |

`{slug}` is the catalog slug: repository base name plus tag for tag references
(`redhat-operator-index-v4.16`), base name only for digest references. The target detail
endpoint lists every catalog with its slug.

## Write endpoints

Used by the console plugin; usable with any token that has `patch`/`delete` on the
resource. Bodies are JSON; a successful call returns `204 No Content`.

| Method | Path | Body |
|---|---|---|
| PATCH | `/api/v1/targets/{namespace}/{name}/spec` | `{registry, insecure, authSecret, concurrency, batchSize, pollInterval, checkExistInterval}` — replaces these fields |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/operators` | List of catalog entries; package filters and `signatureVerification` of kept entries are preserved |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/catalogs/{slug}/packages` | `{packages: [{name, minVersion, maxVersion, channels: [{name, minVersion, maxVersion}]}], exclude: […]}` |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/releases` | `{graph, architectures, channels: [{name, type, minVersion, maxVersion, shortestPath, full}]}` |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/helm` | `helm` section |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/additional-images` | `additionalImages` list |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/blocked-images` | `blockedImages` list |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/settings` | `{requireSignedImages}` |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/recollect` | none — sets the `recollect` annotation |
| PATCH | `/api/v1/imagesets/{namespace}/{name}/force-resync` | none — sets the `force-resync` annotation |
| DELETE | `/api/v1/imagesets/{namespace}/{name}` | none — deletes the ImageSet object (no registry cleanup) |

Responses: `400` invalid body, `401` no bearer token, `403` the token lacks permission, `404` object not
found, `500` other API errors.

## Examples

```bash
URL=https://$(kubectl get route internal-registry-resources -n mirror -o jsonpath='{.spec.host}')

curl -sk $URL/api/v1/targets | jq .
curl -sk $URL/api/v1/targets/internal-registry | jq '.catalogs'
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/idms.yaml | kubectl apply -f -
curl -sk $URL/api/v1/targets/internal-registry/signatures.yaml | kubectl apply -f -

# write with your own token
curl -sk -X PATCH $URL/api/v1/imagesets/mirror/ocp-4-16/recollect \
  -H "Authorization: Bearer $(oc whoami -t)"
```
