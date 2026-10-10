# Registry credentials

`MirrorTarget.spec.authSecret` names a Secret in the same namespace. It is mounted into
the manager, worker, catalog-build and cleanup pods as a Docker credential store, so **one
secret must contain the credentials for every registry involved**: the source registries
you pull from and the target registry you push to.

**Contents**

- [Supported secret formats](#supported-secret-formats)
- [Combining several registries](#combining-several-registries)
- [Reusing the OpenShift pull secret](#reusing-the-openshift-pull-secret)
- [Which pod needs which access](#which-pod-needs-which-access)
- [Rotating credentials](#rotating-credentials)

---

## Supported secret formats

### `kubernetes.io/dockerconfigjson` (recommended)

The standard pull-secret format. One secret can hold any number of registries.

```bash
kubectl create secret docker-registry registry-creds \
  --docker-server=registry.example.com \
  --docker-username=<user> \
  --docker-password=<password> \
  -n oc-mirror-operator
```

### Opaque secret with `username` / `password`

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: registry-creds
  namespace: oc-mirror-operator
type: Opaque
stringData:
  username: robot$mirror
  password: <token>
```

This format can only describe **one** registry and is therefore only practical when
source and target share credentials, or when the sources are anonymous.

## Combining several registries

`kubectl create secret docker-registry` writes exactly one registry. To combine several,
build the `config.json` yourself or log in to each registry with Podman/Docker and use
the resulting file:

```bash
podman login registry.redhat.io
podman login quay.io
podman login registry.example.com

kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=${XDG_RUNTIME_DIR}/containers/auth.json \
  --type=kubernetes.io/dockerconfigjson \
  -n oc-mirror-operator
```

The resulting file looks like this (`auth` is `base64(user:password)`):

```json
{
  "auths": {
    "registry.redhat.io":   { "auth": "…" },
    "quay.io":              { "auth": "…" },
    "registry.example.com": { "auth": "…" }
  }
}
```

Registries not listed are accessed anonymously.

## Merging credentials with `hack/merge-auth.sh`

Manual merging of multiple logins and secrets is error-prone — a missing
registry surfaces much later as a generic `unauthorized` error in the worker
logs or as `CreateContainerConfigError`. The helper script merges sources in
order (later ones win) and can validate the result:

```bash
# Merge the cluster pull secret with the target registry login:
hack/merge-auth.sh -n oc-mirror-operator -o registry-creds \
  --from-secret pull-secret \
  --login registry.example.com robot$mirror s3cr3t \
  --validate --apply

# Or merge local Docker/Podman auth.json files:
hack/merge-auth.sh -n oc-mirror-operator -o registry-creds \
  --from-file ~/.docker/config.json \
  --from-file ${XDG_RUNTIME_DIR}/containers/auth.json \
  --validate --apply
```

`--validate` checks every merged registry entry is non-empty and shaped like
`base64(user:password)` and fails loudly otherwise; `--apply` creates or
replaces the secret, without it the script prints the `kubectl` command.

## Reusing the OpenShift pull secret

The cluster pull secret already contains `registry.redhat.io`, `quay.io` and
`registry.connect.redhat.com`. Extend it with the target registry:

```bash
oc get secret pull-secret -n openshift-config \
  -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d > /tmp/pull-secret.json

# add the target registry
jq --arg auth "$(echo -n 'user:password' | base64 -w0)" \
   '.auths["registry.example.com"] = {"auth": $auth}' /tmp/pull-secret.json > /tmp/combined.json

kubectl create secret generic registry-creds \
  --from-file=.dockerconfigjson=/tmp/combined.json \
  --type=kubernetes.io/dockerconfigjson -n oc-mirror-operator
```

## Which pod needs which access

| Pod | Registries | Access |
|---|---|---|
| Manager | sources (catalog images, release payloads), target | read on sources; read on target for drift checks and the graph image push |
| Worker | sources and target | read on sources, **write** on target |
| Catalog-build Job | source catalog, target | read on source, **write** on target |
| Cleanup Job | target | **delete** on target |

For Quay, use a robot account with `write` permission on the target organization.
Quay creates repositories on first push; the robot account needs the *Create
repositories* permission (or the repositories must exist) — otherwise pushes fail with
`unauthorized` even though the credentials are correct.

## Rotating credentials

Update the secret in place (`kubectl apply` or `kubectl create secret … --dry-run=client
-o yaml | kubectl apply -f -`). The secret is mounted as a volume, so running pods pick
up the new content within about a minute; new worker pods use it immediately. After
fixing wrong credentials, trigger a [recollect](../operations.md#recollect) so
permanently failed images are retried.
