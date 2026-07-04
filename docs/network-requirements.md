# Network Requirements

The oc-mirror-operator components require outbound HTTPS access to several external services.
All connections use port **443** (TLS).

## Required Outbound Domains

| Domain | Port | Component | Purpose | Required? |
|---|---|---|---|---|
| `api.openshift.com` | 443 | Manager pod | Cincinnati upgrade graph — resolves OCP release nodes and edges for the requested channel/version ranges, and (via a separate endpoint) the graph-data archive when `platform.graph: true` | **Required** for OCP release mirroring; **required** for `platform.graph: true` |
| `mirror.openshift.com` | 443 | Manager pod | GPG signatures for OpenShift release images | **Required** for OCP release signature verification — release payloads whose signature cannot be downloaded or fails verification are not mirrored (see below for the opt-out) |
| `quay.io` | 443 | Worker pods | OCP release images (`quay.io/openshift-release-dev/ocp-release`) | **Required** for OCP release mirroring |
| `registry.redhat.io` | 443 | Worker pods | Red Hat operator catalog images and operator bundle images | **Required** for operator mirroring |
| `registry.access.redhat.com` | 443 | Manager pod | UBI9 base image for the Cincinnati graph-data image | **Required** for `platform.graph: true` |
| `api.github.com` | 443 | Console Plugin backend (Dashboard pod) | Queries [`openshift/cincinnati-graph-data`](https://github.com/openshift/cincinnati-graph-data/tree/master/channels) for the current list of available OCP release channels | **Optional** — has ConfigMap and built-in fallbacks (see below) |
| *(target registry)* | 443 / 5000 | Worker pods | User-defined target OCI registry that images are mirrored into | **Required** |
| *(Helm repository hosts)* | 443 | Manager pod | User-defined `spec.mirror.helm.repositories[].url` — chart index and archive downloads | **Required** for Helm chart mirroring |

## Air-Gapped / Restricted Environments

### Channel Discovery Fallback (api.github.com)

The Console Plugin's Release Browser fetches the list of available OCP release channels from the `openshift/cincinnati-graph-data` GitHub repository at startup (cached for 1 hour). In environments without internet access, configure the following fallback:

1. **ConfigMap override** — create a ConfigMap named `oc-mirror-ocp-versions` in the operator namespace:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: oc-mirror-ocp-versions
     namespace: oc-mirror-operator-system
   data:
     # Comma-separated list of OCP minor versions to expose in the UI
     versions: "4.16,4.17,4.18,4.19"
     # Optional: channel types to generate (defaults to all four)
     channelTypes: "stable,fast,eus,candidate"
   ```

2. **Built-in defaults** — if both the GitHub API and the ConfigMap are unavailable, the backend falls back to a hardcoded list (OCP 4.14–4.19, channels: stable / fast / eus / candidate). EUS channels are only generated for even minor versions.

### Release Signature Verification (mirror.openshift.com)

Release payloads are cryptographically verified against the embedded Red Hat
release signing keys before being mirrored — a release node whose signature
cannot be downloaded from `mirror.openshift.com` (or fails verification) is
skipped, not mirrored. In environments without access to `mirror.openshift.com`
(e.g. mirroring unpublished/nightly builds, or genuinely air-gapped setups with
no path to fetch signatures), set `skipSignatureVerification: true` on the
affected channel:

```yaml
spec:
  mirror:
    platform:
      channels:
        - name: stable-4.18
          skipSignatureVerification: true
```

### Cincinnati Graph (api.openshift.com)

In fully disconnected environments you must mirror the Cincinnati graph data separately and point the manager to an internal graph endpoint. Configure the `OcpUpdateURL` environment variable on the Manager deployment.

### OCI Registries (quay.io, registry.redhat.io)

Use `MirrorTarget.spec.registryCredentials` to supply pull secrets. In disconnected environments, configure a local registry mirror and set `MirrorTarget.spec.registryMirrors` accordingly.

## Network Policy Example

If you use Kubernetes `NetworkPolicy` to restrict egress, allow the following rules for each component:

```yaml
# Manager pod egress
- ports:
    - port: 443
  to:
    - ipBlock:
        cidr: 0.0.0.0/0  # or specific CIDRs for api.openshift.com, mirror.openshift.com

# Worker pod egress
- ports:
    - port: 443
  to:
    - ipBlock:
        cidr: 0.0.0.0/0  # or specific CIDRs for quay.io, registry.redhat.io, target registry

# Dashboard/Console Plugin backend egress (optional)
- ports:
    - port: 443
  to:
    - ipBlock:
        cidr: 0.0.0.0/0  # or specific CIDRs for api.github.com
```
