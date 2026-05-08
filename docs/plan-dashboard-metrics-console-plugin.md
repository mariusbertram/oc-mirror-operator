# Plan: Metriken, OAuth-Dashboard & Console Plugin

## Status-Übersicht (Stand: 2026-05-03, Abgeschlossen ✅)

| # | Schritt | Status |
|---|---------|--------|
| 1 | Branch + Plan | ✅ Fertig |
| 2 | Metriken-Grundgerüst | ✅ Fertig |
| 3 | ServiceMonitor + PrometheusRule | ✅ Fertig |
| 4 | Grafana-Dashboard ConfigMap | ✅ Fertig |
| 5 | React-App Scaffolding | ✅ Fertig |
| 6 | Read-only Pages + Frontend API | ✅ Fertig |
| 7 | Edit-Endpunkte + Token-Forwarding | ✅ Fertig |
| 8 | Catalog-Browser UI | ✅ Fertig |
| 9 | Server cluster-wide + cmd/dashboard + Dockerfile | ✅ Fertig |
| 10 | DashboardReconciler + Registrierung | ✅ Fertig (Build OK) |
| 11 | RBAC-Manifeste vervollständigen | ✅ Fertig |
| 12 | Console Plugin Config (Kustomize) | ✅ Fertig |
| 13 | CSV / Bundle Update | ✅ Fertig |
| 14 | oauth-proxy Session-Secret | ✅ Fertig |
| 15 | Tests | ✅ Fertig |

---

## Was ist fertig (details)

### Feature 1: Metriken & Alerts ✅
- `pkg/metrics/controller_metrics.go` — Gauges und Counters für MirrorTarget/ImageSet
- `pkg/metrics/manager_metrics.go` — ManagerRegistry mit `collectors.NewGoCollector()`, `NewManagerMetricsHandler()`
- `internal/controller/mirrortarget_controller.go` — named-return Reconcile, Gauge-Updates, deferred error-counter
- `internal/controller/imageset_controller.go` — deferred error-counter
- `pkg/mirror/manager/manager.go` — `runMetricsServer()` auf `:9090`, Counter-Inkremente für mirrored/failed/batches
- `config/prometheus/manager-monitor.yaml` — ServiceMonitor für Manager-Pods
- `config/prometheus/prometheusrule.yaml` — 5 PrometheusRule Alerts
- `config/grafana/dashboard-configmap.yaml` — vollständiges Grafana-Dashboard JSON

### Feature 2: OAuth-Dashboard ✅ (Hauptteil)
- `ui/` — vollständige React+TypeScript App mit dual Webpack-Build
  - `ui/src/pages/MirrorTargets/` — List + Detail
  - `ui/src/pages/CatalogBrowser/` — Operator-Auswahl mit Checkboxen
  - `ui/src/api/client.ts` — swappable fetch (setFetchImpl), alle API-Funktionen
  - `ui/src/api/types.ts` — TypeScript-Interfaces (inkl. `namespace` in TargetSummary/TargetDetail)
  - `ui/src/dashboard/index.tsx` — SPA Entrypoint
  - `ui/src/plugin/extensions.ts` — Console Plugin Extensions Array
- `pkg/resourceapi/server.go` — Edit-Endpunkte (PATCH catalog-packages, PATCH recollect, DELETE imageset), clientForRequest(), NewServerClusterWide(), lookupMirrorTarget(), RunOn()
- `pkg/resourceapi/plugin/` — Plugin-Manifest Placeholder
- `cmd/dashboard/main.go` — standalone Binary (dashboard + plugin Subkommando)
- `Dockerfile.dashboard` — Multi-stage (Node.js → Go → distroless)
- `Makefile` — `IMG_DASHBOARD`, `build-ui`, `docker-build-dashboard`, `docker-push-dashboard`

### Feature 3: Console Plugin ✅ (Hauptteil)
- `ui/src/plugin/pages/` — MirrorTargetListPage, MirrorTargetDetailPage, CatalogBrowserPage (nutzen consoleFetch)
- `ui/src/plugin/plugin-manifest.json` — Plugin-Manifest
- `pkg/resourceapi/plugin/plugin-manifest.json` — Embed-Placeholder
- `DashboardReconciler` — verwaltet ConsolePlugin CR, Plugin-Deployment, Plugin-Service

### DashboardReconciler ✅ (Code fertig, RBAC unvollständig)
- `internal/controller/dashboard_controller.go` — vollständig implementiert
  - ServiceAccount, ClusterRole, ClusterRoleBinding
  - Dashboard-Deployment mit oauth-proxy Sidecar
  - Service + Route (OpenShift service-CA TLS)
  - Plugin-Deployment + Plugin-Service
  - ConsolePlugin CR (unstructured)
  - Startup-Runnable Bootstrap
- `cmd/controller/main.go` — Registrierung mit Namespace-Guard (OPERATOR_NAMESPACE/POD_NAMESPACE)
- `config/manager/controller.yaml` — `DASHBOARD_IMAGE` und `OPERATOR_NAMESPACE` Env Vars
- `config/manifests/bases/oc-mirror.clusterserviceversion.yaml` — selbe Env Vars

---

## Abgeschlossene Implementierung

### ✅ 11 — RBAC-Manifeste vervollständigen

**Commit: b1eb128** `feat(rbac): add dashboard RBAC rules for clusterroles and consoleplugins`

RBAC-Regeln wurden manuell in `config/rbac/controller_role.yaml` ergänzt:
- `rbac.authorization.k8s.io`: clusterroles, clusterrolebindings (verbs: get, list, watch, create, update, patch)
- `console.openshift.io`: consoleplugins (verbs: get, list, watch, create, update, patch, delete)
- `make manifests` erfolgreich ausgeführt

### ✅ 12 — Console Plugin Kustomize-Config

**Commit: b1eb128** (in RBAC-Commit enthalten)

- ✅ `config/consoleplugin/kustomization.yaml` erstellt
- ✅ `config/default/kustomization.yaml` um `- ../consoleplugin` erweitert
- ✅ Validierung mit `kustomize build config/default` erfolgreich

### ✅ 13 — CSV / Bundle Update

**Commit: 11dce23** `chore(bundle): update bundle with dashboard RBAC and env vars`

`bundle/manifests/oc-mirror.clusterserviceversion.yaml` enthält:
- ✅ `DASHBOARD_IMAGE` env var: `ghcr.io/mariusbertram/oc-mirror-operator-dashboard:latest`
- ✅ `OPERATOR_NAMESPACE` fieldRef: `metadata.namespace`
- ✅ RBAC für `console.openshift.io/consoleplugins`
- ✅ RBAC für `rbac.authorization.k8s.io/clusterroles` und `clusterrolebindings`

### ✅ 14 — oauth-proxy Session-Secret

**Commit: 7945996** `feat(dashboard): add oauth-proxy session secret creation`

- ✅ `ensureOAuthProxySecret()` implementiert in `DashboardReconciler`
- ✅ 32-Byte random session_secret (stabil nach Restart)
- ✅ Idempotent mit `controllerutil.CreateOrUpdate()`
- ✅ Aufgerufen am Anfang von `Reconcile()`

### ✅ 15 — Tests

**Commit: 74fda23** `test(dashboard): add unit tests for DashboardReconciler and resourceapi`

- ✅ Unit-Tests für `DashboardReconciler` (`internal/controller/dashboard_controller_test.go`)
  - Reconcile-Flow: Deployment, Service, Secret, OAuth Proxy
  - ensureClusterRBAC: ServiceAccount, ClusterRole, ClusterRoleBinding
  - ensureOAuthProxySecret: Secret-Erstellung, Stabil ity
  - Deletion Finalizer
- ✅ Unit-Tests für `resourceapi` (`pkg/resourceapi/server_test.go`)
  - LookupMirrorTarget (namespace-bound + cluster-wide)
  - NewServerClusterWide
- ✅ Test-Status: **72/72 Controller + 23/23 ResourceAPI Tests bestanden** ✅
- ✅ Coverage: 71.3% controller statements

---

## Implementierungs-Commits (Übersicht)

```
74fda23 test(dashboard): add unit tests for DashboardReconciler and resourceapi
11dce23 chore(bundle): update bundle with dashboard RBAC and env vars
7945996 feat(dashboard): add oauth-proxy session secret creation
b1eb128 feat(rbac): add dashboard RBAC rules for clusterroles and consoleplugins
d48c43e feat(ui): add React/TS dashboard and console plugin scaffolding with edit API
bb6b94b feat(metrics): add Prometheus metrics for controller, manager and workers
```

---

## Überblick

Drei Features auf einem Branch:

1. **Metriken & Alerts** — Operator, Manager und Worker exponieren Prometheus-Metriken; PrometheusRule für Alerts; Grafana-Dashboard als ConfigMap.
2. **OAuth-Dashboard** — Das bestehende Read-only-UI wird durch eine React-App mit oauth-proxy-Sidecar ersetzt; User editieren ImageSets/MirrorTargets mit ihren eigenen RBAC-Rechten.
3. **Console Plugin** — Dieselbe React-App läuft als OpenShift Console Plugin im Cluster; kein zweites Frontend.

---

## Cross-cutting Decisions

### D1 — Dashboard-Deployment
Das OAuth-Dashboard läuft als **eigenes Deployment** (`oc-mirror-dashboard`) im Operator-Namespace.
Sidecar: `openshift/oauth-proxy`. Port 8443 (oauth-proxy) → Port 8080 (Dashboard-Backend).

### D2 — Auth-Modell: Token-Forwarding
oauth-proxy leitet den OpenShift-User-Token als `X-Forwarded-Access-Token` weiter.
Das Backend baut **pro Request** einen K8s-Client mit diesem Token. Schreibrechte = RBAC des eingeloggten Users.

### D3 — Einheitlicher Frontend-Stack: React/TypeScript
Eine einzige React+TypeScript-App (`ui/`) wird in zwei Artefakten gebaut:
- `dist/dashboard/` — standalone SPA
- `dist/plugin/` — Console-Plugin-Module

### D4 — Worker-Metriken: Aggregation im Manager
Worker melden Ergebnisse über die HTTP-Schiene zurück. Manager führt In-Memory-Counter und exponiert sie auf Port 9090.

### D5 — Edit-API auf `resourceapi`-Server aufbauen
Bestehende Endpunkte bleiben. Neue Edit-Endpunkte + Token-Forwarding im selben Package.
