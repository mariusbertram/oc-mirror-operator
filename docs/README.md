# oc-mirror-operator documentation

The documentation is organised as one path from installation to day-2 operation,
followed by reference material. Read it top to bottom the first time; jump to the
reference pages afterwards.

## Guides

| # | Page | What it answers |
|---|---|---|
| 1 | [Getting started](getting-started.md) | How do I install the operator and get a first mirror running? |
| 2 | [Concepts](concepts.md) | What are MirrorTarget, ImageSet and MirrorExport? Which pods run, and what happens to an image from "declared" to "mirrored"? |
| 3 | [Configuring ImageSets](configuration/imagesets.md) | How do I select OpenShift releases, operator packages, Helm charts and additional images, and exclude images? |
| 4 | [Configuring MirrorTargets](configuration/mirrortarget.md) | How do I configure the target registry, concurrency, polling, proxy, CA bundle, worker storage and exposure? |
| 5 | [Registry credentials](configuration/credentials.md) | Which secret formats are supported and how do I combine several registries? |
| 6 | [Operations](operations.md) | How do I read status and conditions, force a re-resolution or re-transfer, handle failed images, clean up, and monitor with Prometheus? |
| 7 | [Consuming the mirror](consuming-results.md) | How do I get IDMS/ITMS/CatalogSource into the disconnected cluster, use the console plugin and the REST API, and export artifacts for an air gap? |
| 8 | [Troubleshooting](troubleshooting.md) | Symptom → cause → fix for the common failure modes. |
| 9 | [Upgrading](upgrades.md) | How do I upgrade the operator with OLM, and what changed between versions? |

## Reference

| Page | Contents |
|---|---|
| [Architecture](architecture.md) | Components, data flow, state model, catalog build pipeline, blob planning, security model |
| [API reference](reference/api.md) | All CRD fields (`MirrorTarget`, `ImageSet`, `MirrorExport`), annotations, status fields, conditions, generated RBAC |
| [REST API](reference/rest-api.md) | Endpoints of the Resource API and the console plugin backend |
| [Network requirements](reference/network.md) | Outbound endpoints per component and NetworkPolicy notes |
| [Metrics and alerts](reference/metrics.md) | Prometheus metrics, ServiceMonitors, PrometheusRule alerts, dashboard |
| [Design documents](design/) | Longer-form design proposals (state partitioning, signature verification) |

## Contributing

| Page | Contents |
|---|---|
| [Developer guide](developer-guide.md) | Build the binaries and images, deploy to Kind/OpenShift, iterate on a single component, develop the console plugin locally |
| [Contributing guide](contributing.md) | Repository layout, tests, linting, CI pipeline, release process, code style |
| [CHANGELOG](../CHANGELOG.md) | Release notes |

## Conventions used in the docs

- Examples use the namespace `mirror`, a `MirrorTarget` named `internal-registry`, and
  `ImageSet`s named `ocp-4-16-releases` and `ocp-4-16-operators`. Replace them freely.
- `kubectl` is used throughout; `oc` works identically on OpenShift.
- Field paths such as `spec.mirror.operators[].packages[].channels` refer to the CRD
  schema documented in the [API reference](reference/api.md).
