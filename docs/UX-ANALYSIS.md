# UX Analysis — oc-mirror-operator

**Target audience:** experienced OpenShift administrators (day-2 operators of
disconnected/air-gapped clusters). Secondary audience: platform engineers who
need a **0-day bootstrap** of the operator on vanilla Kubernetes or Kind, before
OpenShift exists at all.

**Scope of this analysis:** installation paths, first-run experience,
declarative API ergonomics, status/observability, failure recovery, and
documentation structure. Reviewed against the README, getting-started guide,
quickstarts, configuration reference, troubleshooting and the CRD/API surface.

---

## Persona and context

The primary user is an **OpenShift administrator who has run `oc-mirror` by
hand** and knows registries, IDMS/ITMS, CatalogSources and pull secrets. This
user does not need explanations of what mirroring is — they need:

1. **Speed from install to first successful mirror** (minutes, not hours)
2. **Predictable declarative behavior** — spec edit → observable consequence
3. **Fast root-cause isolation** when something stalls (registry auth,
   network, upstream API, scheduling)
4. **A trustworthy story for the disconnected-cluster consumption side**
   (signatures, IDMS/ITMS, catalog)

The secondary user is doing a **0-day bootstrap**: they have a fresh Kind or
vanilla K8s cluster, no OLM, no OpenShift console, and want the operator
mirroring content into a local registry with the least amount of friction.
This is a fundamentally *different* UX bar: no OperatorHub, no Routes, no
`oc` tooling.

---

## What already works well

- **Two-resource mental model** (`ImageSet` = what, `MirrorTarget` = where) is
  clean and maps directly onto how admins already think about mirroring.
- **Status model is mature**: conditions (`Ready`, `CatalogReady`, `Unbound`),
  the image state machine (`Pending → Mirrored / Failed → PermanentlyFailed`),
  and the troubleshooting doc's symptom-oriented tables are genuinely good.
- **Consumption-side artifacts** (IDMS/ITMS, signatures, CatalogSource) are
  generated automatically — this is the hardest part of the workflow and the
  operator already solves it.
- **Documentation quality is high**: configuration reference with per-registry
  guidance (e.g. `concurrency: 1` for Quay), credential combining recipes,
  network requirements, drift-check semantics.

---

## Findings

Each finding links to a GitHub issue (see the `ux` label) so it can be tracked
on the project board.

### UX-1 — 0-day bootstrap on vanilla K8s requires a Go toolchain and git clone

**Severity: high (blocks the stated secondary audience)**

The only non-OLM install path is:

```bash
git clone … && make install && make deploy IMG=…
```

A platform engineer with a fresh Kind cluster should not need `go`, `make`,
`kustomize` and a source checkout to install a released operator. The fix is a
**published, static, versioned install manifest** (rendered `config/default`
attached to each release, e.g. `oc-mirror-operator.yaml`), plus a short
"Install on plain Kubernetes in 3 commands" doc section. Optionally a Helm
chart as a second distribution channel.

### UX-2 — Bootstrap ordering for the disconnected operator itself is implicit

**Severity: high (this is the chicken-and-egg moment every admin hits)**

The first mirror job in a truly disconnected environment needs the operator's
own images (`controller`, `manager`, `worker`, `plugin`) in the target registry
*before* the operator can run there. The docs mention this only in passing
(`ImagePullBackOff` troubleshooting row). There should be a first-class
**"bootstrap the bootstrapper"** recipe: an `oc-mirror` / `skopeo copy` one-pager
(or a `MirrorExport` artifact listing the operator images with digests) that
mirrors the operator itself, plus the env overrides (`MANAGER_IMAGE`,
`WORKER_IMAGE`, …) pointing at the local registry.

### UX-3 — Credential assembly is the most error-prone manual step

**Severity: medium-high**

One secret must contain *both* source pull creds and target push creds, in
`dockerconfigjson` format, merged by hand (`podman login` × N → copy auth.json).
Failures surface much later as `unauthorized` in worker logs or
`CreateContainerConfigError`. Improvements, in order of impact:

1. A **documented one-liner / helper script** (`hack/merge-auth.sh`) that
   merges a list of `--from-literal` logins or existing secrets.
2. Optional **secret validation in status**: manager probes each referenced
   registry and reports which credential entry failed, instead of a generic
   `unauthorized` at copy time.
3. Longer term: allow **separate `pushSecret` / `pullSecret`** fields on
   `MirrorTarget`.

### UX-4 — Progress visibility during the first (multi-hour) mirror

**Severity: medium**

The first mirror of a release + operator catalog runs for hours. Admins get
conditions and `kubectl logs`, but there is no single
"how far along are we, what is failing right now" view on plain Kubernetes
(the console plugin is OpenShift-only). The Resource API already exposes
image state; exposing a simple **progress summary endpoint**
(`/api/v1/targets/<t>/progress`: total, mirrored, failed, pending, ETA based
on recent throughput) would give a `curl`-able status for Kind users and
could back a CLI one-liner in the docs.

### UX-5 — No OpenShift console on vanilla K8s: the story is undocumented

**Severity: medium**

For the 0-day Kind audience, "console plugin" is meaningless. Docs should
state explicitly: on vanilla K8s, use `kubectl` + Resource API (port-forward
recipe exists) and that's fine. Optionally the plugin could run as a
standalone dashboard (it only reads ConfigMaps via the Resource API), but a
clear doc statement is the cheap fix.

### UX-6 — Documentation sprawl around "getting started"

**Severity: low-medium**

There is `getting-started.md`, five `quickstart-*.md` files, and overlapping
setup steps across them (prerequisites, namespace, credentials are repeated
in every quickstart with slight variations, e.g. `mirror` vs
`oc-mirror-operator` namespace inconsistency). An experienced admin hits
ambiguity ("which doc is canonical?"). Consolidate common setup into one
page and have quickstarts link to it; pick one example namespace everywhere.

### UX-7 — `pollInterval`/`checkExistInterval` minimum of 1h is surprising

**Severity: low**

Experienced admins testing the operator on Kind want fast feedback and set
`pollInterval: 5m` — which is silently rejected. Consider allowing smaller
minimums (or making the minimum a validation *warning* rather than a clamp),
at least for the drift-check interval, and document the clamp behavior
prominently next to the field.

### UX-8 — Error text → cause mapping is good in docs, invisible in the cluster

**Severity: low**

The troubleshooting doc's error-text tables are excellent, but an admin
staring at `kubectl get imageset` sees `Ready=False` with a terse reason.
Events or condition messages that embed the doc's symptom keywords
("unauthorized", "i/o timeout", "signature verification") verbatim from the
worker/manager log lines would shorten the path from symptom to doc entry.

---

## Suggested project board layout

| Column | Suggested items |
|---|---|
| Backlog | UX-5, UX-7, UX-8 |
| Ready | UX-3, UX-4, UX-6 |
| In progress | UX-1, UX-2 (unblock the 0-day bootstrap story first) |
| Done | — |

Issues are labeled `ux` and cross-referenced from this document.
