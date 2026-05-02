# Plan: Metriken, OAuth-Dashboard & Console Plugin

## Überblick

Drei Features auf einem Branch:

1. **Metriken & Alerts** — Operator, Manager und Worker exponieren Prometheus-Metriken; PrometheusRule für Alerts; Grafana-Dashboard als ConfigMap.
2. **OAuth-Dashboard** — Das bestehende Read-only-UI wird durch eine React-App mit oauth-proxy-Sidecar ersetzt; User editieren ImageSets/MirrorTargets mit ihren eigenen RBAC-Rechten.
3. **Console Plugin** — Dieselbe React-App läuft als OpenShift Console Plugin im Cluster; kein zweites Frontend.

---

## Cross-cutting Decisions

Diese Entscheidungen gelten übergreifend für alle drei Features.

### D1 — Dashboard-Deployment

Das OAuth-Dashboard läuft als **eigenes Deployment**, das der `MirrorTargetReconciler` **cluster-weit** (nicht pro MirrorTarget) verwaltet. Begründung: Die Edit-UI muss alle MirrorTargets/ImageSets in allen Namespaces sehen; einen Manager-Pod dafür umzubauen würde das Manager-Design sprengen.

Deployment: `oc-mirror-dashboard` im Operator-Namespace  
Sidecar: `openshift/oauth-proxy` (Konfiguration via Annotation, OpenShift-Service-CA-Cert)  
Port 4180 (oauth-proxy) → Port 8082 (Dashboard-Backend)

### D2 — Auth-Modell: Token-Forwarding, nicht SA-Token

oauth-proxy leitet den OpenShift-User-Token als `X-Forwarded-Access-Token` Header an das Dashboard-Backend weiter.

Das Dashboard-Backend baut **pro Request** einen `k8s.io/client-go`-Client mit diesem Bearer-Token. Damit greifen die nativen RBAC-Rechte des eingeloggten Users für alle K8s-Writes (ImageSet erstellen/ändern, MirrorTarget patchen). Das Backend besitzt **keinen eigenen Write-SA**; der SA des Dashboards hat nur List/Watch-Rechte (für Health-Checks).

Gleiches Schema für das Console Plugin: Console leitet den Session-Token als `Authorization: Bearer` an den Plugin-Backend-Service weiter.

### D3 — Einheitlicher Frontend-Stack: React/TypeScript

Das aktuelle Vanilla-HTML/JS-UI (`pkg/resourceapi/ui/`) wird **ersetzt**. Eine einzige React+TypeScript-App (`ui/`) wird in zwei Artefakten gebaut:

- `dist/dashboard/` — standalone SPA (für das OAuth-Dashboard)
- `dist/plugin/` — Console-Plugin-Module (via `@openshift-console/dynamic-plugin-sdk`)

Der Build-Schritt läuft in einem Multi-stage Container-Build. Das Go-Backend bettet `dist/dashboard/` via `//go:embed` ein.

### D4 — Worker-Metriken: Aggregation im Manager

Worker-Pods sind ephemer (Sekunden bis Minuten) und können von Prometheus nicht zuverlässig gescrapt werden. Worker melden Ergebnisse bereits über die HTTP-Schiene an den Manager zurück. Neu: Der Manager führt In-Memory-Counter (via `prometheus/client_golang`) und exponiert diese an einem dedizierten `/metrics`-Endpoint im Manager-Pod. Der Controller verwaltet einen ServiceMonitor pro MirrorTarget/Manager-Pod.

Kein Pushgateway — keine neue Cluster-Abhängigkeit.

### D5 — Edit-API auf `resourceapi`-Server aufbauen

Die bestehenden Endpunkte in `pkg/resourceapi/server.go` bleiben erhalten. Neue Edit-Endpunkte werden im selben Package hinzugefügt. Das Token-Forwarding (D2) wird als HTTP-Middleware implementiert, die den Client per Request austauscht.

---

## Feature 1: Metriken & Alerts

### 1.1 Operator Controller (`:8443/metrics`, bereits vorhanden)

Neue Custom Metrics registrieren in `internal/controller/`:

| Metric | Typ | Labels | Quelle |
|--------|-----|--------|--------|
| `oc_mirror_mirrortarget_images_total` | Gauge | `namespace`, `target` | `MirrorTarget.status.totalImages` |
| `oc_mirror_mirrortarget_images_mirrored` | Gauge | `namespace`, `target` | `MirrorTarget.status.mirroredImages` |
| `oc_mirror_mirrortarget_images_failed` | Gauge | `namespace`, `target` | `MirrorTarget.status.failedImages` |
| `oc_mirror_mirrortarget_images_pending` | Gauge | `namespace`, `target` | `MirrorTarget.status.pendingImages` |
| `oc_mirror_reconcile_errors_total` | Counter | `namespace`, `target`, `controller` | Fehler in Reconcile-Loop |
| `oc_mirror_imageset_last_poll_seconds` | Gauge | `namespace`, `imageset` | `ImageSet.status.lastSuccessfulPollTime` |

Implementierung: `pkg/metrics/controller_metrics.go` — Prometheus-Registry via `sigs.k8s.io/controller-runtime/pkg/metrics` (kein eigener Registry nötig).

### 1.2 Manager-Pod (`:9090/metrics`, neu)

Neuer HTTP-Listener im Manager auf Port 9090 (getrennt von API-Port 8081).

Metriken in `pkg/metrics/manager_metrics.go`:

| Metric | Typ | Labels |
|--------|-----|--------|
| `oc_mirror_manager_batches_total` | Counter | `target`, `result` (`success`/`failed`) |
| `oc_mirror_manager_images_mirrored_total` | Counter | `target`, `imageset` |
| `oc_mirror_manager_images_failed_total` | Counter | `target`, `imageset` |
| `oc_mirror_manager_batch_duration_seconds` | Histogram | `target` |
| `oc_mirror_manager_active_workers` | Gauge | `target` |
| `oc_mirror_manager_worker_retries_total` | Counter | `target` |

Manager registriert Metriken beim Start; Worker-Status-Callbacks (`reportSuccess`/`reportError`) inkrementieren die Counter.

### 1.3 ServiceMonitors

- Bestehender ServiceMonitor für Controller bleibt, Labels werden ergänzt.
- Neuer ServiceMonitor `manager-metrics-monitor` in `config/prometheus/manager-monitor.yaml` — selektiert Manager-Pods via Label `app.kubernetes.io/component: manager`, `oc-mirror.openshift.io/mirrortarget: <name>`.
- Der `MirrorTargetReconciler` erstellt/löscht den ServiceMonitor zusammen mit dem Manager-Deployment (mittels `controllerutil.CreateOrUpdate()`).

### 1.4 PrometheusRule (Alerts)

Datei: `config/prometheus/prometheusrule.yaml`

| Alert | Bedingung | Severity |
|-------|-----------|----------|
| `OCMirrorHighFailedImages` | `oc_mirror_mirrortarget_images_failed > 10` für 5 min | warning |
| `OCMirrorAllImagesFailed` | `oc_mirror_mirrortarget_images_failed / oc_mirror_mirrortarget_images_total > 0.5` für 10 min | critical |
| `OCMirrorReconcileErrors` | `rate(oc_mirror_reconcile_errors_total[5m]) > 0` | warning |
| `OCMirrorNoProgress` | `oc_mirror_mirrortarget_images_pending > 0` und `rate(oc_mirror_manager_images_mirrored_total[30m]) == 0` für 30 min | warning |
| `OCMirrorManagerDown` | `absent(oc_mirror_manager_active_workers)` für 5 min | critical |

### 1.5 Grafana-Dashboard

Datei: `config/grafana/dashboard-configmap.yaml`

ConfigMap mit Label `grafana_dashboard: "1"` (kompatibel mit Grafana-Operator Community-Standard) sowie `console.openshift.io/dashboard: "true"` (OpenShift Monitoring Stack).

Panels:
- Overview: Total/Mirrored/Pending/Failed per Target (Gauge-Panes)
- Timeline: Mirroring-Throughput (images/min), Fehlerrate
- Per-Target-Drill-Down: Batch-Dauer Histogram, aktive Worker, Retry-Rate
- Alerts-Panel: Aktive Alerts aus PrometheusRule

---

## Feature 2: OAuth-Dashboard mit Edit-Funktionen

### 2.1 React-App (`ui/`)

Neue Verzeichnisstruktur:

```
ui/
  src/
    components/       # Shared PF5 Komponenten
    pages/
      MirrorTargets/  # List + Detail
      ImageSets/      # List + Edit (inkl. Catalog Browser)
      CatalogBrowser/ # Operator-Auswahl aus Upstream-Catalog
    api/              # fetch-Wrapper, Token-handling
    plugin/           # Console-Plugin Entrypoint (dynamic-plugin-sdk)
    dashboard/        # Standalone Dashboard Entrypoint
  package.json
  tsconfig.json
  webpack.plugin.js
  webpack.dashboard.js
```

### 2.2 Edit-Flows

**Catalog-Browser / Operator-Auswahl:**
1. Fetch `GET /api/v1/targets/{mt}/catalogs/{slug}/upstream-packages.json` — liefert alle verfügbaren Packages.
2. Fetch `GET /api/v1/targets/{mt}/catalogs/{slug}/packages.json` — liefert aktuell gefilterte Packages.
3. UI zeigt Checkbox-Liste. Änderungen werden als `PATCH` an einen neuen Edit-Endpunkt gesendet.

**Neue Edit-Endpunkte** in `pkg/resourceapi/server.go`:

```
PATCH /api/v1/imagesets/{namespace}/{name}/catalogs/{slug}/packages
      Body: {"include": ["pkg-a", "pkg-b"], "exclude": ["pkg-c"]}

PATCH /api/v1/imagesets/{namespace}/{name}/recollect
      (setzt mirror.openshift.io/recollect Annotation)

POST  /api/v1/imagesets/{namespace}/{name}
      (erstellt neues ImageSet, Body: ImageSpec)

DELETE /api/v1/imagesets/{namespace}/{name}
       (löscht ImageSet)
```

Alle Endpunkte extrahieren den Bearer-Token aus `X-Forwarded-Access-Token` (Dashboard) oder `Authorization` (Console Plugin) und bauen einen Request-scoped K8s-Client.

### 2.3 Dashboard-Deployment

Das Deployment wird vom `MirrorTargetReconciler` **nicht** verwaltet — es ist cluster-weit und gehört nicht zu einem einzelnen MirrorTarget. Stattdessen: neuer `DashboardReconciler` im Controller, der ein einzelnes Deployment im Operator-Namespace sichert.

Kubernetes-Ressourcen:

```
Deployment: oc-mirror-dashboard
  containers:
    - name: dashboard
      image: DASHBOARD_IMAGE (env var wie OPERATOR_IMAGE)
      port: 8082
    - name: oauth-proxy
      image: openshift/oauth-proxy:latest
      args:
        - --upstream=http://localhost:8082
        - --tls-cert=/etc/tls/private/tls.crt
        - --tls-key=/etc/tls/private/tls.key
        - --cookie-secret-file=/etc/proxy/secrets/session_secret
        - --openshift-service-account=oc-mirror-dashboard
        - --openshift-sar={"namespace":"{{.Namespace}}","resource":"mirrortargets","verb":"list"}
        - --pass-access-token=true
      port: 4180

ServiceAccount: oc-mirror-dashboard
  annotations:
    serviceaccounts.openshift.io/oauth-redirectreference.dashboard: >
      {"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"oc-mirror-dashboard"}}

Service: oc-mirror-dashboard (port 4180 → oauth-proxy)

Route: oc-mirror-dashboard (TLS edge, terminiert an Service)

Secret: oc-mirror-dashboard-proxy (session_secret, random 32 Bytes)
```

### 2.4 RBAC

- `oc-mirror-dashboard` SA bekommt `imageset-viewer-role` + `mirrortarget-viewer-role` als Cluster-Mindestrechte (nur für Health-Check-Zwecke).
- Tatsächliche Schreibrechte liegen beim eingeloggten User-Token (D2).
- Neue `imageset-editor-role` ClusterRole existiert bereits — keine Änderung nötig.

---

## Feature 3: OpenShift Console Plugin

### 3.1 ConsolePlugin-Ressource

```yaml
apiVersion: console.openshift.io/v1alpha1
kind: ConsolePlugin
metadata:
  name: oc-mirror-plugin
spec:
  displayName: "OC Mirror"
  service:
    name: oc-mirror-plugin
    namespace: <operator-namespace>
    port: 9443
    basePath: /
```

### 3.2 Plugin-Backend-Service

Neues eigenständiges Binary `cmd/dashboard/main.go` (dient beiden Zwecken: Dashboard-Backend und Console-Plugin-Backend):

```
GET  /plugin-manifest.json   (Console-Plugin-Manifest)
GET  /static/                (Plugin-Bundle-Assets, via embed)
API-Endpunkte: wie Dashboard (selbes Package pkg/resourceapi)
```

Der Service ist von der Console erreichbar. NetworkPolicy wird entsprechend geöffnet.

### 3.3 Plugin-Frontend

Die React-App in `ui/src/plugin/` registriert Pages und Extensions via `@openshift-console/dynamic-plugin-sdk`:

- Extension: `console.navigation/section` (OC Mirror Sektion in Navigation)
- Extension: `console.page/route` (MirrorTargets, ImageSets)
- Extension: `console.action/resource-provider` (Aktionen auf MirrorTarget/ImageSet-Seiten)

Funktionsgleich mit dem Dashboard: dieselben React-Komponenten, nur anderes Routing/Auth-Handling.

### 3.4 Authentifizierung im Plugin

Console leitet den User-Token als `Authorization: Bearer <token>` an Plugin-API-Calls weiter (Standard-SDK-Verhalten via `consoleFetch`). Das Backend-Handling (D2) ist identisch mit dem Dashboard.

---

## Implementierungsreihenfolge

1. **Branch + Plan** (jetzt) ✓
2. **Metriken-Grundgerüst** — `pkg/metrics/` Package, Controller-Metriken, Manager-Metrics-Endpoint
3. **ServiceMonitor + PrometheusRule** — Kustomize-Config, Manager-Reconciler-Integration
4. **Grafana-Dashboard ConfigMap**
5. **React-App Scaffolding** — `ui/` initialisieren, Webpack-Setup (dual build), PF5-Grundstruktur
6. **Read-only Pages migrieren** — MirrorTargets/ImageSets Listenansichten (ersetzt vanilla UI)
7. **Edit-Endpunkte im resourceapi** — Token-Middleware, PATCH/POST/DELETE Handler
8. **Catalog-Browser UI** — Operator-Auswahl, Checkbox-Liste, PATCH-Flow
9. **oauth-proxy Dashboard-Deployment** — DashboardReconciler, Route, Secret-Rotation
10. **Console Plugin** — ConsolePlugin CR, Plugin-Manifest, Plugin-Entrypoint
11. **Bundle/CSV update** — neue SAs, Permissions, ConsolePlugin
12. **Tests** — Controller-Tests für DashboardReconciler, Unit-Tests für neue API-Handler, E2E-Smoke

---

## Abhängigkeiten / neue Images

| Image | Zweck | Wer baut |
|-------|-------|----------|
| `DASHBOARD_IMAGE` | Dashboard + Plugin Backend + eingebettetes Frontend | Multi-stage Dockerfile, CI |
| `openshift/oauth-proxy` | oauth-proxy Sidecar | Upstream, kein Build nötig |

Kein Pushgateway, kein neuer Operator-Framework-Dependency.

## Was explizit nicht gemacht wird

- Kein separates Frontend für Dashboard und Plugin — eine Codebase.
- Kein Pushgateway.
- Kein Write-SA für das Dashboard-Backend.
- Bundle-Inhalte nicht manuell editieren — `make bundle` nach allen Änderungen.
