# Getting started

This guide takes you from an empty cluster to a mirrored OpenShift release plus one
operator, and shows how a disconnected cluster consumes the result. It takes about
15 minutes of hands-on time; the mirroring itself runs in the background.

**Contents**

1. [Prerequisites](#1-prerequisites)
2. [Install the operator](#2-install-the-operator)
3. [Create the registry credentials](#3-create-the-registry-credentials)
4. [Declare what to mirror: ImageSet](#4-declare-what-to-mirror-imageset)
5. [Declare where to mirror: MirrorTarget](#5-declare-where-to-mirror-mirrortarget)
6. [Watch the mirror fill up](#6-watch-the-mirror-fill-up)
7. [Consume the mirror from a disconnected cluster](#7-consume-the-mirror-from-a-disconnected-cluster)
8. [Where to go next](#8-where-to-go-next)

---

## 1. Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes ≥ 1.26 or OpenShift ≥ 4.12 | The console plugin and `Route` exposure need OpenShift; everything else works on plain Kubernetes. |
| OLM ≥ 0.22 | Only for the OLM-based installation. |
| A target registry with write access | Quay, Harbor and `registry:2` (Docker Distribution) are tested. |
| Network access from the cluster | The manager pod needs `api.openshift.com` and `mirror.openshift.com`; worker pods need the source registries (`quay.io`, `registry.redhat.io`, …) and the target registry. See [Network requirements](reference/network.md). |
| Credentials | A pull secret for the source registries (for Red Hat content: your `registry.redhat.io` pull secret) and push credentials for the target registry. |

The operator needs **no persistent storage**. All mirroring state is kept in
gzip-compressed ConfigMaps.

## 2. Install the operator

### Option A: OLM (recommended)

The operator is published in the `brtrm-dev-catalog` catalog (package `oc-mirror`).

```bash
# 1. Register the catalog (namespace: openshift-marketplace on OpenShift, olm elsewhere)
cat <<EOT | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: brtrm-dev-catalog
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: quay.io/mariusbertram/brtrm-dev-catalog:latest
  displayName: brtrm Dev Catalog
EOT

# 2. Namespace, OperatorGroup and Subscription
kubectl create namespace oc-mirror-operator
cat <<EOT | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: oc-mirror-operator
  namespace: oc-mirror-operator
spec:
  targetNamespaces: [oc-mirror-operator]
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: oc-mirror
  namespace: oc-mirror-operator
spec:
  name: oc-mirror
  channel: alpha              # kubectl get packagemanifest oc-mirror -n openshift-marketplace -o jsonpath='{.status.channels[*].name}'
  source: brtrm-dev-catalog
  sourceNamespace: openshift-marketplace
EOT

# 3. Wait for the CSV
kubectl get csv -n oc-mirror-operator -w
```

On OpenShift you can do the same through **Operators → OperatorHub** in the web
console; search for *oc-mirror*.

> The operator is **namespace-scoped**: it watches `MirrorTarget` and `ImageSet`
> resources in its own namespace only. Create your mirror resources there, or install the
> operator into the namespace you want to use. The examples below use the operator
> namespace `oc-mirror-operator` as `mirror`; pick one and stay consistent.

### Option B: Plain manifests

```bash
git clone https://github.com/mariusbertram/oc-mirror-operator.git
cd oc-mirror-operator
make install                                   # CRDs
make deploy IMG=ghcr.io/mariusbertram/oc-mirror-operator-controller:<version>
```

`make deploy` renders `config/default` with kustomize; the controller Deployment carries the
`MANAGER_IMAGE`, `WORKER_IMAGE`, `PLUGIN_IMAGE` and `OPERATOR_IMAGE` environment variables
that tell it which images to use for the pods and jobs it creates.

### Verify

```bash
kubectl get pods -n oc-mirror-operator
# NAME                                              READY   STATUS
# oc-mirror-operator-controller-manager-...         1/1     Running
kubectl get crd | grep mirror.openshift.io
# imagesets.mirror.openshift.io
# mirrorexports.mirror.openshift.io
# mirrortargets.mirror.openshift.io
```

## 3. Create the registry credentials

One secret of type `kubernetes.io/dockerconfigjson` holds the credentials for **all**
registries involved — the sources you pull from and the target you push to. The easiest
way is to start from a Docker/Podman `config.json` that already contains them:

```bash
export NS=oc-mirror-operator

podman login registry.redhat.io          # or docker login
podman login registry.example.com        # the target

kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${XDG_RUNTIME_DIR}/containers/auth.json \
  --type=kubernetes.io/dockerconfigjson -n $NS
```

Other ways to build the secret (OpenShift pull secret, username/password) are described in
[Registry credentials](configuration/credentials.md).

## 4. Declare what to mirror: ImageSet

An `ImageSet` lists content. This one mirrors the latest patch releases of OpenShift 4.16
starting at 4.16.20 and the `web-terminal` operator:

```yaml
# imageset.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-16
  namespace: oc-mirror-operator
spec:
  mirror:
    platform:
      architectures: [amd64]
      channels:
        - name: stable-4.16
          minVersion: "4.16.20"          # every release ≥ 4.16.20 in the channel
    operators:
      - catalog: registry.redhat.io/redhat/redhat-operator-index:v4.16
        packages:
          - name: web-terminal            # heads-only: newest bundle of every channel
    additionalImages:
      - name: registry.redhat.io/ubi9/ubi:latest
```

```bash
kubectl apply -f imageset.yaml
```

Nothing happens yet: an `ImageSet` on its own is inert until a `MirrorTarget` references it.

## 5. Declare where to mirror: MirrorTarget

```yaml
# mirrortarget.yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
  namespace: oc-mirror-operator
spec:
  registry: registry.example.com/mirror     # host[:port]/path, no scheme
  authSecret: registry-creds
  imageSets:
    - ocp-4-16
```

```bash
kubectl apply -f mirrortarget.yaml
```

The controller now creates a **manager** Deployment named `internal-registry-manager`.
The manager resolves the `ImageSet` (Cincinnati graph, catalog FBC), writes the image
list into the ConfigMap `ocp-4-16-images`, and starts **worker** pods that copy the
images in batches. Once every image of the `ImageSet` is mirrored, a **catalog-build**
Job produces the filtered `redhat-operator-index` image.

## 6. Watch the mirror fill up

```bash
kubectl get mirrortarget,imageset -n oc-mirror-operator
# NAME                                              TOTAL   MIRRORED   PENDING   FAILED   AGE
# mirrortarget.mirror.openshift.io/internal-registry   412     87         325       0        4m
# NAME                                  TOTAL   MIRRORED   PENDING   FAILED   AGE
# imageset.mirror.openshift.io/ocp-4-16   412     87         325       0        4m

kubectl get pods -n oc-mirror-operator -w              # manager, workers, catalog-build job
kubectl logs deployment/internal-registry-manager -n oc-mirror-operator -f
kubectl get imageset ocp-4-16 -n oc-mirror-operator -o jsonpath='{.status.conditions}' | jq
```

You are done when `PENDING` is `0` and the `ImageSet` shows
`CatalogReady=True`. Failed images, if any, are listed in `status.failedImageDetails`;
[Operations → Failed images](operations.md#failed-images) explains what to do about them.

Meanwhile, the mirror keeps itself up to date: every 24 h (`pollInterval`) the manager
checks upstream for new releases and bundles, and every 6 h (`checkExistInterval`) it
verifies the target registry still holds every mirrored image.

## 7. Consume the mirror from a disconnected cluster

The manager generates the cluster resources a consumer needs and the Resource API serves
them over HTTP. On OpenShift a `Route` is created automatically:

```bash
URL=https://$(kubectl get route internal-registry-resources -n oc-mirror-operator -o jsonpath='{.spec.host}')

# Image mirror rules (apply on the disconnected cluster)
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/idms.yaml | kubectl apply -f -
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/itms.yaml | kubectl apply -f -

# Release signatures, required for cluster upgrades against the mirror
curl -sk $URL/api/v1/targets/internal-registry/signatures.yaml | kubectl apply -f -

# The filtered operator catalog (OLM v0)
curl -sk $URL/api/v1/targets/internal-registry/imagesets/ocp-4-16/catalogs/redhat-operator-index-v4.16/catalogsource.yaml | kubectl apply -f -
```

On plain Kubernetes use `spec.expose.type: Ingress` or port-forward the
`oc-mirror-resource-api` Service on port 8081. All options are covered in
[Consuming the mirror](consuming-results.md).

## 8. Where to go next

- [Concepts](concepts.md) — understand what just happened under the hood.
- [Configuring ImageSets](configuration/imagesets.md) — version ranges, channels,
  `previousVersions`, Helm charts, blocked images, signature requirements.
- [Configuring MirrorTargets](configuration/mirrortarget.md) — concurrency, intervals,
  proxy, CA bundle, large-image storage, cleanup policy.
- [Operations](operations.md) — recollect, force-resync, cleanup, monitoring.
