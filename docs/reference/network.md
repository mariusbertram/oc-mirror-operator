# Network requirements

All outbound connections are HTTPS (TCP 443) unless the target registry uses another
port.

## Outbound endpoints

| Endpoint | Component | Purpose | Needed when |
|---|---|---|---|
| `api.openshift.com` | manager | Cincinnati upgrade graph (`/api/upgrades_info/v1/graph`); graph-data archive for the OSUS image | `platform.channels` / `platform.graph` |
| `mirror.openshift.com` | manager | GPG signatures of release payloads (`/pub/openshift-v4/signatures/…`) | release channels without `skipSignatureVerification` |
| `quay.io` | manager, worker, catalog-build | Release payloads and component images (`openshift-release-dev/*`), many operator images | release mirroring; catalogs referencing quay.io |
| `registry.redhat.io`, `registry.access.redhat.com`, `registry.connect.redhat.com` | manager, worker, catalog-build | Operator catalogs and bundle/related images; UBI base image for the graph image | operator mirroring; `platform.graph` |
| Helm repository hosts | manager | `index.yaml` and chart archives | `helm.repositories` |
| Any other source registry in the spec | manager, worker | manifests (manager) and blobs (worker) | `additionalImages`, third-party catalogs |
| **Target registry** | manager, worker, catalog-build, cleanup | push, `HEAD` for drift checks, delete for cleanup | always |
| `api.github.com` | console plugin backend | List of OCP release channels for the release editor | optional, see below |

The manager also needs the Kubernetes API; workers need the manager Service
(`<target>-manager.<ns>.svc:8080`) and DNS.

## Proxies and private CAs

Configure `MirrorTarget.spec.proxy` and `spec.caBundle` — see
[Configuring MirrorTargets](../configuration/mirrortarget.md#http-proxy). The
controller pod does not use `spec.proxy`; give it proxy settings through the OLM
Subscription (`spec.config.env`) if it must reach the API server through a proxy.

## Air-gapped: channel discovery

The console plugin's release editor fetches the list of channels from the
`openshift/cincinnati-graph-data` repository on GitHub. Without internet access it falls
back to a ConfigMap, then to a built-in list:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: oc-mirror-ocp-versions
  namespace: oc-mirror-operator        # operator namespace
data:
  versions: "4.16,4.17,4.18,4.19"
  channelTypes: "stable,fast,eus,candidate"   # optional
```

## Air-gapped: everything else

The operator itself needs a path to the upstream sources; it is a mirroring tool, not
an archive tool. For a fully disconnected target, run the operator in a connected
(or DMZ) cluster that can reach both sides, or use a `MirrorExport` to render the image
list and hand the copy to other tooling. Cincinnati can be pointed at an internal
graph endpoint by setting `OcpUpdateURL` in the manager Deployment
(`pkg/mirror/release.OcpUpdateURL`).

## NetworkPolicies created by the operator

| Policy | Selector | Rule |
|---|---|---|
| `<target>-manager-ingress` | `app=oc-mirror-manager, mirrortarget=<target>` | Ingress on 8080 only from that target's workers; 8081 and 9090 from anywhere in-cluster |
| `<target>-worker-ingress-deny` | `app=oc-mirror-worker, mirrortarget=<target>` | No ingress |

Egress is deliberately unrestricted because DNS and registry topologies differ too much
between clusters. To restrict worker egress yourself, target the worker selector and
allow DNS, the manager Service and the registries:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: internal-registry-worker-egress
spec:
  podSelector:
    matchLabels: { app: oc-mirror-worker, mirrortarget: internal-registry }
  policyTypes: [Egress]
  egress:
    - to: [{ namespaceSelector: {}, podSelector: { matchLabels: { app: oc-mirror-manager } } }]
      ports: [{ port: 8080 }]
    - ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    - ports: [{ port: 443 }]          # narrow to registry CIDRs where possible
```
