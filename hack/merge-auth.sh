#!/usr/bin/env bash
# merge-auth.sh — build the combined registry credential Secret for
# MirrorTarget.spec.authSecret from multiple sources.
#
# One kubernetes.io/dockerconfigjson secret must hold the credentials for
# EVERY registry involved: the sources you pull from and the target you
# push to. This script merges (in order):
#
#   1. an existing dockerconfigjson Secret from the cluster
#   2. one or more local Docker/Podman auth.json files
#   3. inline user:password pairs for a registry
#
# into a single config.json and prints the kubectl command (or applies it).
#
# Usage:
#   hack/merge-auth.sh -n <namespace> -o <secret-name> [sources...]
#
# Sources (any combination, order matters — later ones win):
#   --from-secret <name>        read an existing kubernetes.io/dockerconfigjson secret
#   --from-file <path>           merge a Docker/Podman config.json / auth.json file
#   --login <registry> <user> <password>   add one registry credential inline
#   --validate                   after building, verify each registry entry is non-empty
#   --apply                      create/replace the secret instead of printing the command
#
# Examples:
#   # Merge the cluster pull secret with the target registry login:
#   hack/merge-auth.sh -n mirror -o registry-creds \
#     --from-secret pull-secret \
#     --login registry.example.com robot$mirror s3cr3t
#
#   # Merge two local auth.json files and a live secret:
#   hack/merge-auth.sh -n mirror -o registry-creds \
#     --from-secret registry-creds \
#     --from-file ~/.docker/config.json \
#     --from-file ${XDG_RUNTIME_DIR}/containers/auth.json \
#     --validate --apply
set -euo pipefail

NAMESPACE=""
SECRET_NAME=""
FROM_SECRET=""
FROM_FILES=()
LOGINS=()
VALIDATE="false"
APPLY="false"

usage() { grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage ;;
    -n|--namespace) NAMESPACE="$2"; shift 2 ;;
    -o|--output) SECRET_NAME="$2"; shift 2 ;;
    --from-secret) FROM_SECRET="$2"; shift 2 ;;
    --from-file) FROM_FILES+=("$2"); shift 2 ;;
    --login) LOGINS+=("$2" "$3" "$4"); shift 4 ;;
    --validate) VALIDATE="true"; shift ;;
    --apply) APPLY="true"; shift ;;
    *) echo "ERROR: unknown option: $1" >&2; usage ;;
  esac
done

[ -n "$NAMESPACE" ] || { echo "ERROR: --namespace is required" >&2; usage; }
[ -n "$SECRET_NAME" ] || { echo "ERROR: --output is required" >&2; usage; }
command -v jq >/dev/null || { echo "ERROR: jq is required" >&2; exit 1; }

MERGED='{"auths":{}}'

merge_auths() {
  # merge_auths <config.json content> — merges its .auths into $MERGED
  MERGED=$(jq -n --argjson base "$MERGED" --argjson add "$1" \
    '$base * {auths: ($base.auths + $add.auths)}')
}

if [ -n "$FROM_SECRET" ]; then
  kubectl get secret "$FROM_SECRET" -n "$NAMESPACE" >/dev/null 2>&1 \
    || { echo "ERROR: secret $FROM_SECRET not found in namespace $NAMESPACE" >&2; exit 1; }
  SECRET_JSON=$(kubectl get secret "$FROM_SECRET" -n "$NAMESPACE" \
    -o jsonpath='{.data\.dockerconfigjson}' | base64 -d)
  merge_auths "$SECRET_JSON"
fi

for f in "${FROM_FILES[@]:-}"; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || { echo "ERROR: file not found: $f" >&2; exit 1; }
  merge_auths "$(cat "$f")"
done

i=0
while [ $i -lt ${#LOGINS[@]} ]; do
  reg="${LOGINS[$i]}"
  j=$((i+1))
  k=$((i+2))
  user="${LOGINS[$j]}"
  pass="${LOGINS[$k]}"
  auth=$(printf '%s:%s' "$user" "$pass" | base64 -w0)
  MERGED=$(jq -n --argjson base "$MERGED" --arg reg "$reg" --arg auth "$auth" \
    '$base * {auths: ($base.auths + {($reg): {auth: $auth}})}')
  i=$((i+3))
done

ENTRIES=$(jq -r '.auths | keys[]' <<<"$MERGED" | wc -l)
if [ "$ENTRIES" -eq 0 ]; then
  echo "ERROR: no credentials merged — provide at least one source" >&2
  exit 1
fi

if [ "$VALIDATE" = "true" ]; then
  echo "Merged registries:"
  FAIL=0
  while read -r reg; do
    auth=$(jq -r --arg r "$reg" '.auths[$r].auth // empty' <<<"$MERGED")
    if [ -z "$auth" ]; then
      echo "  ✗ $reg has an empty auth entry"
      FAIL=1
    else
      # Decode and check user:password shape.
      if ! decoded=$(printf '%s' "$auth" | base64 -d 2>/dev/null) \
         || [[ "$decoded" != *:* ]]; then
        echo "  ✗ $reg auth is not valid base64(user:password)"
        FAIL=1
      else
        echo "  ✓ $reg"
      fi
    fi
  done < <(jq -r '.auths | keys[]' <<<"$MERGED")
  [ "$FAIL" -eq 0 ] || { echo "ERROR: validation failed" >&2; exit 1; }
fi

TMP=$(mktemp)
jq . <<<"$MERGED" > "$TMP"

if [ "$APPLY" = "true" ]; then
  kubectl create secret generic "$SECRET_NAME" \
    --from-file=.dockerconfigjson="$TMP" \
    --type=kubernetes.io/dockerconfigjson \
    -n "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
  echo "Applied secret $SECRET_NAME in namespace $NAMESPACE"
else
  echo "Run the following to create the secret:"
  echo "  kubectl create secret generic $SECRET_NAME \\"
  echo "    --from-file=.dockerconfigjson=$TMP \\"
  echo "    --type=kubernetes.io/dockerconfigjson -n $NAMESPACE"
fi
rm -f "$TMP"
