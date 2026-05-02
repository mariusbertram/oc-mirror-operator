# Plan: Metriken, OAuth-Dashboard & Console Plugin

## Status-Übersicht (Stand: 2026-05-03)

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
| 11 | RBAC-Manifeste vervollständigen | ⚠️ Offen |
| 12 | Console Plugin Config (Kustomize) | ⚠️ Offen |
| 13 | CSV / Bundle Update | ⚠️ Offen |
| 14 | oauth-proxy Session-Secret | ⚠️ Offen |
| 15 | Tests | ⚠️ Offen |

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

## Offene Punkte

### ⚠️ 11 — RBAC-Manifeste vervollständigen

`controller-gen` übernimmt die neuen Markers aus `dashboard_controller.go` **nicht zuverlässig** für `rbac.authorization.k8s.io/clusterroles;clusterrolebindings` und `console.openshift.io/consoleplugins`. Der Grund ist noch unklar (möglicher Bug in controller-gen v2 für diese Ressourcentypen).

**Lösung**: Fehlende Regeln manuell in `config/rbac/controller_role.yaml` ergänzen:
```yaml
- apiGroups:
  - rbac.authorization.k8s.io
  resources:
  - clusterroles
  - clusterrolebindings
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
- apiGroups:
  - console.openshift.io
  resources:
  - consoleplugins
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
```

### ⚠️ 12 — Console Plugin Kustomize-Config

Fehlt noch:
- `config/consoleplugin/kustomization.yaml`
- Eintrag in `config/default/kustomization.yaml`

Die ConsolePlugin CR und Plugin-Deployment werden vom DashboardReconciler dynamisch erstellt — kein separater Kustomize-Eintrag nötig. Aber der RBAC für `console.openshift.io` in der CSV muss vorhanden sein (→ Punkt 11).

### ⚠️ 13 — CSV / Bundle Update

Nach Änderung der RBAC-Manifeste (Punkt 11):
```bash
make bundle IMG=<image>
```

Prüfen ob `bundle/manifests/oc-mirror.clusterserviceversion.yaml` enthält:
- `DASHBOARD_IMAGE` Env Var
- `OPERATOR_NAMESPACE` fieldRef
- RBAC für `console.openshift.io/consoleplugins`
- RBAC für `rbac.authorization.k8s.io/clusterroles`

Optional: Annotation `features.operators.openshift.io/openshift-console-plugin: "true"` in CSV.

### ⚠️ 14 — oauth-proxy Session-Secret

Der DashboardReconciler setzt voraus, dass `oc-mirror-dashboard-proxy` Secret existiert (enthält `session_secret`). Das Secret wird **nicht** automatisch erstellt.

**Lösung**: In `ensureOAuthProxySecret()` ein Secret mit 32-Byte Zufallswert anlegen (nur wenn nicht vorhanden, damit es nach Restart stabil bleibt):
```go
func (r *DashboardReconciler) ensureOAuthProxySecret(ctx context.Context) error {
    secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: dashboardName + "-proxy", Namespace: r.Namespace}}
    _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
        if secret.Data == nil {
            secret.Data = map[string][]byte{}
        }
        if _, exists := secret.Data["session_secret"]; !exists {
            b := make([]byte, 32)
            _, _ = rand.Read(b)
            secret.Data["session_secret"] = b
        }
        return nil
    })
    return err
}
```

### ⚠️ 15 — Tests

Folgende Tests fehlen noch:
- Unit-Tests für `DashboardReconciler` (analog `mirrortarget_controller_test.go`)
- Unit-Tests für neue `resourceapi` Handler (`handlePatchCatalogPackages`, `handleTriggerRecollect`, `handleDeleteImageSet`)
- Unit-Tests für `NewServerClusterWide` + `lookupMirrorTarget`

---

## Nächste Schritte (empfohlene Reihenfolge)

1. **RBAC manuell in `controller_role.yaml` ergänzen** → `make manifests` + commit
2. **`ensureOAuthProxySecret()` im DashboardReconciler** ergänzen → commit  
3. **`make bundle`** ausführen um CSV zu aktualisieren → prüfen + commit
4. **Tests** schreiben (minimal: DashboardReconciler, lookupMirrorTarget)
5. **npm install + build** lokal testen (`make build-ui`)
6. End-to-End smoke test auf einem Cluster

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
