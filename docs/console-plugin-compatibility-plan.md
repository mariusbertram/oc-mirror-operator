# Versioned Console Plugin Compatibility Plan

## Goal and agreed scope

Deliver five Console Plugin variants for OpenShift **4.18, 4.19, 4.20, 4.21,
and 4.22**. Each must match the Console's SDK, React, PatternFly, and Router
runtime. The operator selects the image from the Console ClusterOperator version.
Keep the implementation on `feat/versioned-console-plugin-ux`, preserve existing
work, regenerate `bundle/` from `config/`, and deliver a pull request.

This document is the persistent multi-session plan and completion record.
Older compatibility profiles are out of scope; older releases in sample mirror
content are intentionally retained.

## Completed session-sized execution plan

| Session | Scope | Result |
|---|---|---|
| 1: Reliable profile installation | Compatibility matrix, installer, TypeScript configs and Router adapters | One npm transaction resolves the whole selected profile; checked-in manifest and default lock remain unchanged. All runtime and build-tool versions are exact and asserted after installation. Real selected adapters type-check, with no ambient declaration masking them. |
| 2: Rendering and local development | UI pages, styles, PatternFly adapters, webpack and mock harness | PF5 uses legacy Modal title/actions; PF6 uses ModalHeader/Body/Footer. All five optimized production builds pass. Windows default/mock mode renders all eight routes and supports dialog/write-action acknowledgements without a cluster. |
| 3: Operator selection and permissions | ConsolePlugin reconciler/tests, entrypoints and generated RBAC | Selects only 4.18-4.22, handles upgrades and missing/unsupported versions/images, preserves deployments on transient API errors, retries cleanup, and watches ClusterOperator changes. Focused controller specs and complete `make test` passed; controller coverage was 91.2%. |
| 4: Delivery and runtime matrix | Dockerfile, Makefile, Kustomize, CSV, CI/release workflows and smoke harness | Five image/env/relatedImage mappings render correctly. Bundle regenerated/validated through existing tooling. Release waits for all five variants and asserts digest/env alignment. Actual matching Console 4.18-4.22 browser runs all passed with optimized production assets. |
| 5: Handoff and PR | Developer guide, changelog, this plan and branch delivery | Documentation reflects verified behavior and platform limits. Implementation commit `26d061e` pushed to the feature branch; [PR #197](https://github.com/mariusbertram/oc-mirror-operator/pull/197) opened. |

### Follow-up: light and dark theme regressions

Completed the same five-version real Console matrix in both Light and Dark:
90 page checks, 10,120 rendered-text contrast samples, and 280 screenshots.
The harness chooses the supported Console preference in the Preferences UI,
reloads, and proves persistence/classes/tokens while the emulated OS preference
is deliberately opposite. Fixtures are recreated between themes so confirmed
Delete and Force Resync actions cannot contaminate the second run.

Plugin-page-scoped token mapping now uses actual PF6 semantic tokens and PF5
fallbacks without redefining Console root variables. Native selects/options,
catalog panes/selected rows/hover actions, status text, borders/backgrounds,
dialogs, and filter/Refresh toolbars were exercised in both themes. Measured
low-contrast light-mode warning text and hovered imported-package text were
corrected. All five optimized builds and actual-adapter typechecks passed.

Normal rendered text is checked at 4.5:1, large text at 3:1, and meaningful
selected-row indicators at 3:1. Decorative borders are checked for correct
token resolution rather than forced to 3:1. Headless Chromium cannot establish
the pixel contrast of every browser/OS-native popup; computed select/option
styles and `color-scheme` are checked instead. Disabled controls are exempt
from the rendered-text measurements. This is not a complete accessibility audit.

## Validation record

Executed on 2026-10-03 using isolated container-local dependency/source trees:

- Five exact profile installs, five TypeScript checks of actual adapters, and five
  optimized production webpack builds succeeded.
- Five real `quay.io/openshift/origin-console` bridges against a dedicated Kind
  cluster passed browser validation: eight distinct routes, nine page checks,
  22 screenshots per version, detail tabs, dialog headings/body/footer/Cancel,
  authenticated Recollect/Force Resync/Delete, and catalog interactions. Resource
  API responses were real, not mocked. Expected unrelated OpenShift-shell noise
  on vanilla Kubernetes is reported separately by the harness.
- The runtime runs uncovered missing underscore-prefixed webpack chunks in the
  Go embed. `//go:embed all:plugin` fixes this. The asset regression test compares
  every emitted file with its served contents and passed for each production
  profile; the complete resource API package suite also passed.
- Go formatting, focused controller tests (35 specs), full `make test`, and
  entrypoint compilation passed with Go 1.27.1 and envtest.
- Both Kustomize renders, bundle regeneration/validation, cluster-wide
  ClusterOperator permissions, five image/env references, and synthetic
  digest-pinned CSV assertions passed.
- A clean default Windows npm install, lint (zero errors; three existing hook
  dependency warnings), TypeScript, and live mock browser checks passed.
- Follow-up toolbar spacing uses shared PF5/PF6 horizontal/vertical gaps with
  responsive wrapping on Mirror Targets, ImageSets, and Pending & Failed Images.
  Both PF5 and PF6 mock browsers passed layout measurements at 1280, 640, and
  480 pixels; screenshots were reviewed. Real-Console smoke assertions now guard
  the same spacing and wrapping.

Published release images/digests are intentionally not created by this feature
branch. The tag-triggered release workflow resolves published image tags through
`USE_IMAGE_DIGESTS=true` and rejects a bundle if any variant is missing, unpinned,
or differs from its matching deployment environment value.

## Acceptance checklist

- [x] Exactly five profiles and image variants: 4.18-4.22.
- [x] Installed dependencies retain every selected exact version.
- [x] Actual Router and PatternFly adapters type-check.
- [x] Five isolated optimized production builds succeed.
- [x] Five matching real Console browser runs succeed, including dialogs/actions.
- [x] Both Console-selected Light/Dark themes pass for all five versions while overriding opposite OS preferences.
- [x] Clean default local mock mode works on Windows without cluster access.
- [x] Version-selection, upgrade, cleanup and failure-path Go tests pass.
- [x] Generated RBAC grants cluster-wide ClusterOperator access.
- [x] Kustomize and regenerated bundle preserve image/env references.
- [x] Release workflow enforces digest pinning and corresponding env alignment.
- [x] Documentation covers supported versions, overrides, failures and startup.
- [x] Scoped commit pushed and [PR #197](https://github.com/mariusbertram/oc-mirror-operator/pull/197) created.

## Reusable validation commands

```bash
npm --prefix ui ci
npm --prefix ui run lint
npx --prefix ui tsc --noEmit -p ui/tsconfig.json
npm --prefix ui run dev:mock

# Production assets: Linux only; repeat for all five versions.
CONSOLE_VERSION=4.18 npm --prefix ui run install:console-profile
npx --prefix ui tsc --noEmit -p ui/tsconfig.console.json
CONSOLE_VERSION=4.18 npm --prefix ui run build:plugin

make test
go test ./pkg/resourceapi -count=1
make manifests bundle USE_IMAGE_DIGESTS=false
```

Run matching-console browser validation through `plugin-smoke-test` in
`.github/workflows/ci.yml`; `hack/plugin-smoke/seed.mjs` supplies repeatable data,
and `smoke-test.mjs` writes screenshots and JSON reports. Matrix artifacts have
version-specific names.

## Operational constraints and resolved blockers

- Never share mounted `ui/node_modules` between Linux and Windows, or install
  different profiles concurrently. Previous shared-tree webpack CLI failures
  were reproduced successfully with clean isolated dependencies.
- Never uninstall Router types in a second npm transaction: it can reset a
  profile to default dependencies. The installer now synthesizes one temporary
  complete manifest, restores it even on failure, and records installed versions.
- The standalone harness deliberately uses the default React 17 / Router 5
  profile. Restore it with `npm --prefix ui ci` after a production profile install.
- Production plugin builds use Linux because SDK dynamic-module paths are
  platform-specific. Windows mock development is verified and does not use that
  federation plugin.
- SDK 1.4.0 / webpack SDK 1.1.1 follow the 4.18 template; the exact pair built and
  rendered successfully against the real 4.18 Console.
- Production mode now follows webpack's `--mode production`, enabling hashed,
  minified assets rather than depending on an unset shell `NODE_ENV`.
- Mock actions acknowledge writes; they do not mutate persisted cluster/sample
  data. The real-console smoke harness separately tests actual authorized writes.

## Follow-up design consideration: minc

Evaluated minc/MicroShift as an optional cluster beneath a separately installed
Console. Retain the existing matching-version real Console bridge smoke matrix
on Kind: it already exercises actual federation. Mock mode is only a standalone
development aid and cannot prove Console federation compatibility.

MicroShift's ingress router/service CA could provide useful additional
routing/certificate coverage. However, adding a standalone Console does not
establish the full OCP console-operator, ClusterOperator version lifecycle,
automatic plugin registration, or authentication integrations. Those require
full OpenShift coverage, not manually supplied compatibility resources. No minc
benchmark or new cluster installation was performed for this design review,
and no CI replacement is authorized or made.

Before adopting minc in CI, identify a concrete uncovered integration, validate
the required components and versions, and compare reliability and cost with the
existing smoke suite. See the developer guide's test-environment discussion.

## Resume checklist

The pending-and-failed image page is now named **Pending & Failed Images**, or
**Pending & Failed Images - &lt;target&gt;** when scoped (the UI uses an em dash).
This includes permanently failed images that are not actively queued.
Navigation, manifests, standalone harness, descriptions, documentation and smoke
expectations use this terminology. Existing `/failed` and `/failures` URLs and
internal exposed-module names are retained; genuine **Failed** state labels and
counters are unchanged.

1. Read this plan and `git status`; preserve the feature branch and existing work.
2. Check the acceptance list and PR status before inventing further work.
3. For UI changes rerun affected profile builds and real matching-console smoke
   tests; build/typecheck alone cannot establish federation compatibility.
4. For controller/RBAC changes rerun existing envtest and generation targets.
5. For delivery changes render both install and CSV configurations, regenerate
   the bundle, and validate all five image/env/digest mappings.
