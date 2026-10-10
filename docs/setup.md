# Setup: Install the Operator and Registry Credentials

This is the **canonical setup guide** — every quickstart and the
getting-started guide link here instead of repeating the steps. Do this once,
then follow the guide for your specific use case.

Throughout the documentation the example namespace is
`oc-mirror-operator`. The operator is namespace-scoped: create your
`ImageSet` and `MirrorTarget` resources in the same namespace the operator
watches.

---

## 1. Install the Operator

Choose one installation method.

### Option A: OLM (recommended for OpenShift)

```bash
# 1. Create namespace
kubectl create namespace oc-mirror-operator

# 2. Register the catalog
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

# 3. Install the operator
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
  channel: alpha
  source: brtrm-dev-catalog
  sourceNamespace: openshift-marketplace
EOT
```

On OpenShift you can do the same through **Operators → OperatorHub** in the
web console; search for *oc-mirror*.

### Option B: Static release manifest (plain Kubernetes)

Every release publishes a rendered install manifest — works on vanilla
Kubernetes and Kind without Go/make/kustomize:

```bash
kubectl apply -f https://github.com/mariusbertram/oc-mirror-operator/releases/download/v<version>/oc-mirror-operator.yaml
kubectl rollout status deployment/oc-mirror-controller-manager -n oc-mirror-system
```

### Option C: From source

```bash
git clone https://github.com/mariusbertram/oc-mirror-operator.git
cd oc-mirror-operator
make install                                   # CRDs
make deploy IMG=ghcr.io/mariusbertram/oc-mirror-operator-controller:<version>
```

### Verify

```bash
kubectl get pods -n oc-mirror-system
# NAME                                              READY   STATUS
# oc-mirror-controller-manager-...                   1/1     Running
kubectl get crd | grep mirror.openshift.io
# imagesets.mirror.openshift.io
# mirrorexports.mirror.openshift.io
# mirrortargets.mirror.openshift.io
```

For disconnected installs of the operator itself see
[Bootstrap the Bootstrapper](quickstart-bootstrap-operator.md).

## 2. Prepare the Registry Credentials

One secret of type `kubernetes.io/dockerconfigjson` holds the credentials
for **all** registries involved — the sources you pull from and the target
you push to:

```bash
export NS=oc-mirror-operator
podman login registry.redhat.io          # or docker login
podman login registry.example.com        # the target
kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${XDG_RUNTIME_DIR}/containers/auth.json \
  --type=kubernetes.io/dockerconfigjson -n $NS
```

Reference it as `MirrorTarget.spec.authSecret: registry-creds`.

More options — OpenShift pull secret reuse, opaque `username`/`password`
secrets, validation and merging multiple sources with
`hack/merge-auth.sh` — are described in
[Registry credentials](configuration/credentials.md).

---

**Next:** follow the guide for your use case in
[Quickstarts](quickstart.md) or the [Getting Started](getting-started.md)
walkthrough.
