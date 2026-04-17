# Task Status: oc-mirror-operator

## Phase 1: Architektur & Kern-Logik (Abgeschlossen)
- [x] Initialisierung des Kubebuilder-Projekts mit API-Gruppe `mirror.openshift.io`.
- [x] Implementierung des Manager-Worker-Modells (Operator spawnt Manager, Manager spawnt Worker).
- [x] OCI-basiertes State Management (Metadaten in der Ziel-Registry speichern).
- [x] Multi-Architektur Build-Unterstützung via Podman (Cross-Compilation).
- [x] Ressourcen-Konfiguration für Manager/Worker in der `MirrorTarget` CR.

## Phase 2: Cincinnati Integration (Abgeschlossen)
- [x] Schlanke Cincinnati-Anbindung zur Auflösung von OpenShift Releases.
- [x] Parsing von Release-Graphen zur Ermittlung von Payload-Images.
- [x] Integration in den `Collector`.

## Phase 3: Operator Filtering (In Arbeit/Grundlagen vorhanden)
- [x] Grundgerüst für FBC (File-Based Catalog) Filtering im `Collector`.
- [x] Spiegelung des Katalog-Images selbst.
- [ ] Vollständige Implementierung des In-Memory FBC Parsings (ohne externe Pakete).

## Phase 4: Feedback & Stabilität (Abgeschlossen)
- [x] Feedback-Loop von Worker zu Manager via Log-Parsing (`RESULT_DIGEST`).
- [x] Automatische Status-Updates in `ImageSet` Ressourcen.
- [x] Umfassendes Refactoring zur Vermeidung von zirkulären Importen (`pkg/mirror/client`).

## Phase 5: Dokumentation & Tests (In Arbeit)
- [x] Umfassende `README.md` mit Architekturdiagramm-Beschreibung.
- [x] Unit-Tests für `state`, `worker` und `release` Pakete.
- [x] Testabdeckung > 75% für kritische Logikpfade.
- [ ] Integration-Tests mit Fake-Cluster.

---

## Code Review Findings (2026-04-17)

Schwerpunkte: Architektur · Security · Implementierung

### 🔴 KRITISCH — Blocker

#### [IMPL-01] Compile-Fehler in `cmd/main.go`
`go build` schlägt mit 5 Fehlern fehl. Das Projekt ist im aktuellen Zustand nicht kompilierbar.
- `"github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"` importiert aber nicht als `mirrorv1alpha1` genutzt → `undefined: mirrorv1alpha1`
- `mirror.NewMirrorClient` existiert nicht im `mirror`-Package (liegt in `pkg/mirror/client`) → `undefined: mirror.NewMirrorClient`
- `context` und `fmt` fehlen in den Imports → `undefined: context`, `undefined: fmt`

**Fix:** Korrekte Imports ergänzen; `mirror.NewMirrorClient()` → `mirrorclient.NewMirrorClient()`.

---

#### [IMPL-02] State Management ist ein Stub — inkrementelles Mirroring funktioniert nicht
`pkg/mirror/state/state.go`: `ReadMetadata` ignoriert Repository und Tag vollständig (`_ = repository`) und gibt immer leere Metadaten zurück. `WriteMetadata` schreibt nichts und gibt `"sha256:dummy"` zurück.

Effekt: Jeder Reconcile-Lauf hält alle Images für neu. Bereits gespiegelte Images werden nicht erkannt. Die gesamte Deduplizierungs- und Inkrementallogik ist wirkungslos.

**Fix:** Echte OCI-Blob-Implementierung mit `regclient` umsetzen (Manifest + Blob Push/Pull).

---

#### [IMPL-03] CatalogResolver ist ein Stub — Operator-Images werden nicht aufgelöst
`pkg/mirror/catalog/resolver.go`: `ResolveCatalog` gibt ausschließlich das Catalog-Image selbst zurück. FBC-Parsing, Bundle-Image-Extraktion und Package-Filterung sind nicht implementiert.

**Fix:** FBC aus dem Catalog-Image extrahieren (OCI-Layer pull), `declcfg` parsen und `RelatedImages` je Bundle sammeln. `FilterFBC` ist bereits vorhanden und korrekt.

---

### 🟠 HOCH — Security

#### [SEC-01] Registry-Credentials werden nie an Worker-Pods übergeben
`pkg/mirror/manager/manager.go` → `startWorker()`: Worker-Pods werden ohne jegliche Auth-Konfiguration gestartet. `MirrorTarget.Spec.AuthSecret` wird in der API definiert, aber an keiner Stelle gemountet oder als Umgebungsvariable übergeben.

Gleiches gilt für `cmd/main.go` → `runWorker()`: Der Kommentar `// Todo: handle insecure and auth` belegt, dass Auth komplett fehlt. Das Flag `--insecure` wird zwar geparst, aber nicht genutzt.

**Fix:** `AuthSecret` als Volume in den Worker-Pod mounten (`/root/.docker/config.json` oder als `REGISTRY_AUTH_FILE`) und den `MirrorClient` entsprechend konfigurieren.

---

#### [SEC-02] Log-Injection-Angriff über `RESULT_DIGEST` + unzuverlässige Tier-Kommunikation
`pkg/mirror/manager/manager.go` → `getDigestFromLogs()`: Der Digest des gespiegelten Images wird durch Parsen von `stdout` des Worker-Pods extrahiert (`RESULT_DIGEST=<wert>`).

**Security:** Ein manipuliertes Source-Image könnte beliebige Strings in `stdout` schreiben und einen gefälschten Digest in die State-Metadaten einschleusen. Damit kann ein Angreifer ein beliebiges Image als "erfolgreich gespiegelt" markieren.

**Reliability:** Die Log-basierte Kommunikation versagt zusätzlich in folgenden Szenarien: Logs werden rotiert bevor der Manager sie liest; Pod wird gelöscht bevor Logs abgerufen werden (Race Condition im `cleanupPods`-Flow, der Pod direkt nach dem Status-Check löscht).

**Fix:** Workers sollen Ergebnisse in eine dedizierte CRD (`MirrorTask`) oder in ein `Status`-Feld schreiben. Alternativ: Digest serverseitig via Registry-API verifizieren (`MirrorClient.GetDigest()` bereits vorhanden). Log-Parsing für sicherheitskritische Werte vollständig eliminieren.

---

#### [SEC-03] Hardcoded Service-Account mit überbreiten Berechtigungen im Manager-Pod
`internal/controller/mirrortarget_controller.go` Zeile 77:
```go
ServiceAccountName: "oc-mirror-operator-controller-manager",
```
Der Manager-Pod (der pro `MirrorTarget` als Deployment gestartet wird) erhält den Service-Account des Operators, der cluster-weit Pods erstellen/löschen, Secrets lesen und Deployments verwalten darf (`config/rbac/role.yaml`). Das verletzt das Prinzip der minimalen Rechtevergabe. Worker-Pods erben den `default`-SA ohne explizite Einschränkung.

**Fix:** Dedizierten Service-Account für Manager-Pods mit minimalem RBAC erstellen. Worker-Pods keinen SA mounten (`automountServiceAccountToken: false`).

---

#### [SEC-04] `CONTROLLER_IMAGE` ohne Validierung direkt in Pod-Specs
`pkg/mirror/manager/manager.go` Zeile 58–61: Wenn `CONTROLLER_IMAGE` nicht gesetzt ist, wird `controller:latest` verwendet — ein generisches Image-Tag ohne Digest-Pinning. Über diese Umgebungsvariable kann das Worker-Image zur Laufzeit beliebig überschrieben werden.

**Fix:** Image-Referenz mit SHA256-Digest pinnen. Leeren Wert als Fehler behandeln, nicht stillschweigend mit einem Default überschreiben.

---

#### [SEC-05] HTTP-Client ohne Timeout für Cincinnati-API
`pkg/mirror/release/release.go` Zeile 44: `http.DefaultClient.Do(req)` — `http.DefaultClient` hat keinen Timeout. Bei einem nicht-antwortenden Cincinnati-Endpunkt blockiert der Goroutine indefinit.

**Fix:** `http.Client{Timeout: 30 * time.Second}` verwenden.

---

#### [SEC-06] `ClusterRole` für namespace-scoped Operator
`config/rbac/role.yaml`: Die generierte Rolle ist eine `ClusterRole`. Sie erlaubt cluster-weiten Zugriff auf Secrets (`get;list;watch`). Ein namespace-scoped Operator sollte eine `Role` mit Binding auf seinen Namespace verwenden.

**Fix:** `ClusterRole` → `Role` + `RoleBinding` im Operator-Namespace.

---

### 🟡 MITTEL — Architektur & Design

#### [ARCH-01] Manager nutzt Polling statt event-driven Reconciliation
`pkg/mirror/manager/manager.go`: Der Manager-Prozess läuft als simpler 30-Sekunden-Ticker-Loop (`time.NewTicker`). Das widerspricht dem Kubebuilder-Reconciler-Muster und führt zu unnötigen API-Server-Anfragen, selbst wenn sich nichts geändert hat.

**Fix:** Manager als eigenen `controller-runtime`-Controller implementieren, der auf `ImageSet`-Events reagiert.

---

#### [ARCH-02] `WorkerPool` (`pkg/mirror/worker.go`) ist Dead Code
`worker.go` implementiert einen vollständigen Goroutine-basierten Worker-Pool, der aber nirgendwo instanziiert oder genutzt wird. Stattdessen startet der Manager Kubernetes-Pods. Diese Duplizierung schafft Verwirrung über das tatsächliche Architekturmodell.

**Fix:** `worker.go` entweder entfernen oder aktiv nutzen (z.B. für den In-Cluster Worker-Pfad).

---

#### [ARCH-03] Dualer Write-Pfad für `ImageSet`-Status
Sowohl der `ImageSetReconciler` (controller) als auch der `MirrorManager` (Deployment) schreiben direkt auf `.Status` von `ImageSet`-Objekten. Das führt zu konkurrierenden Updates und kann zu `ResourceVersion`-Konflikten und inkonsistenten Status-Feldern führen.

**Fix:** Status-Writes ausschließlich über den Controller-Manager-Reconciler kanalisieren. Der Manager-Pod sollte den Status über eigene CR-Ressourcen (z.B. `MirrorTask`) signalisieren, auf die der Controller reagiert.

---

#### [ARCH-04] Fehlende Finalizer für Cleanup-Logik
Weder `MirrorTarget` noch `ImageSet` registrieren Finalizer. Beim Löschen einer `MirrorTarget`-CR wird das zugehörige Manager-Deployment über den `ownerReference`-Garbage-Collector aufgeräumt, aber laufende Worker-Pods (ohne Owner-Referenz, siehe IMPL-05) bleiben als Waisen zurück.

**Fix:** Finalizer für `MirrorTarget` implementieren, der Worker-Pods und Metadaten in der Registry bereinigt.

---

#### [ARCH-06] In-Memory Coordinator State — Nicht persistent bei Pod-Restart
`pkg/mirror/manager/manager.go`: Die `inProgress`-Map (`map[string]string`) liegt ausschließlich im Speicher des Manager-Pods. Wird der Manager-Pod neugestartet (z.B. durch ein Node-Drain, OOMKill oder Deployment-Update), verliert er den kompletten Überblick über laufende Worker-Pods.

Folge: Der Manager startet für bereits laufende oder abgeschlossene Worker neue Pods, was zu Duplikaten und Race Conditions beim Status-Update führt.

**Fix:** `inProgress`-State in den `ImageSet.Status` (z.B. neues `State: "InProgress"`) oder eine dedizierte CRD (`MirrorTask`) persistieren. Beim Start des Managers den Zustand aus dem Cluster rekonstruieren (laufende Worker-Pods mit Label-Selector abfragen).

---

#### [ARCH-07] Lineare Performance-Degradierung bei der Status-Aktualisierung
`pkg/mirror/manager/manager.go` → `updateImageStatus()` und `reconcile()`: Bei jeder Reconcile-Iteration werden alle `ImageSet`-Objekte im Namespace aufgelistet und vollständig iteriert, um das passende Image anhand des `Destination`-Strings zu finden. Die Laufzeitkomplexität ist O(ImageSets × Images).

Mit wachsender Anzahl an `ImageSets` und zu spiegelnden Images wird der Manager-Pod zum Bottleneck.

**Fix:** Kubernetes Indexer in controller-runtime nutzen (`mgr.GetFieldIndexer().IndexField(...)` auf `spec.targetRef`), um direkt auf relevante `ImageSet`-Objekte zuzugreifen. Alternativ einen dedizierten `MirrorTask`-Controller implementieren.

---
`internal/controller/imageset_controller.go` Zeile 68:
```go
if len(is.Status.TargetImages) == 0 {
```
Die Image-Liste wird nur einmalig befüllt. Ändert sich `spec.mirror`, wird die Soll-Liste nie neu berechnet. Es gibt keine Generation-Tracking oder Spec-Hash-Vergleich.

**Fix:** `metadata.generation` oder einen Spec-Hash in den Status schreiben und bei Änderung neu berechnen.

---

### 🟡 MITTEL — Implementierung

#### [IMPL-04] Alle Operator-Images landen an derselben Destination
`pkg/mirror/collector.go` Zeile 71:
```go
dest := fmt.Sprintf("%s/operator-catalog:%s", target.Spec.Registry, "latest")
```
Alle Operatoren aus allen Katalogen teilen denselben Ziel-Tag. Jeder Operator überschreibt den vorherigen Eintrag in der Status-Liste (gleiche `Destination`).

**Fix:** Destination aus dem Catalog-Image-Namen ableiten (z.B. `registry/namespace/catalog-name:tag`).

---

#### [IMPL-05] Worker-Pods haben keine Owner-Referenz
`pkg/mirror/manager/manager.go` → `startWorker()`: Worker-Pods werden ohne `ownerReferences` erstellt. Fällt der Manager-Pod aus oder wird der er neu gestartet, sind laufende Worker-Pods Waisen und werden nie aufgeräumt.

**Fix:** `controllerutil.SetControllerReference` auf den Worker-Pod setzen (Owner = Manager-Pod oder `MirrorTarget`).

---

#### [IMPL-06] Fehler in `release.go` werden stillschweigend ignoriert
Drei Fehler werden mit `_` verworfen:
- `u, _ := url.Parse(OcpUpdateURL)` (Zeile 36)
- `req, _ := http.NewRequestWithContext(...)` (Zeile 44)
- `body, _ := io.ReadAll(resp.Body)` (Zeile 57)

Fehler beim Request-Build führen zu einem `nil`-Pointer-Panic. Fehler beim Body-Lesen werden ignoriert und führen zu leerem/korruptem JSON-Unmarshal.

**Fix:** Alle Fehler explizit behandeln und zurückgeben.

---

#### [IMPL-07] Release-Resolver ignoriert `MinVersion`, `ShortestPath` und `Full`
`pkg/mirror/release/release.go`: Es wird immer nur exakt die angegebene `MaxVersion` zurückgegeben. Die API-Felder `MinVersion`, `ShortestPath` und `Full` aus `ReleaseChannel` werden nie ausgelesen oder übergeben.

**Fix:** Cincinnati-Graph traversieren und den Versions-Bereich entsprechend der konfigurierten Felder auflösen (inkl. Shortest-Path-Berechnung über Graph-Edges).

---

#### [IMPL-11] Fehler werden nicht in CRD-Conditions sichtbar gemacht
Mirroring-Fehler (fehlgeschlagene Worker, Registry-Timeouts, Resolver-Fehler) werden ausschließlich in `stdout`/`stderr` des Manager-Pods geloggt. `ImageSet.Status.Conditions` wird nie mit Fehlerinformationen befüllt. Nutzer können Fehler nicht über `kubectl describe imageset` diagnostizieren.

**Fix:** Robuste `Status.Conditions`-Updates für `ImageSet` und `MirrorTarget` implementieren. Fehlerzustände (z.B. `Type: Degraded`, `Type: Progressing`) mit sprechenden `Reason`- und `Message`-Feldern setzen. `apimeta.SetStatusCondition()` aus `k8s.io/apimachinery` nutzen.

---

#### [IMPL-12] Ungültige Go-Version in `go.mod`
`go.mod` Zeile 3: `go 1.25.7` — Diese Version existiert nicht. Das Go-Versionsschema verwendet zweistellige Minor-Versionen (z.B. `1.22.4`, `1.23.0`). Die aktuelle Version im Dockerfile ist `golang:1.26`, was ebenfalls eine Fantasieversion ist.

**Fix:** Tatsächlich verwendete Go-Version eintragen (z.B. `go 1.23.4`) und Dockerfile-Base-Image entsprechend anpassen. `go mod tidy` ausführen um Konsistenz sicherzustellen.

---
Images, die in den Status `Failed` übergehen, bleiben dauerhaft in diesem Zustand. Der Manager überspringt sie in der Reconcile-Schleife (nur `Pending` wird verarbeitet). Es gibt keine Retry-Logik, kein exponentielles Backoff und kein konfigurierbares Retry-Limit.

**Fix:** Retry-Counter und `NextRetryAt`-Zeitstempel im `TargetImageStatus` ergänzen.

---

#### [IMPL-09] `hasChanged`-Bedingung für Metadata-Write ist fehlerhaft
`pkg/mirror/manager/manager.go` Zeile 166:
```go
if hasChanged || len(m.inProgress) == 0 {
```
Metadaten werden auch dann geschrieben, wenn sich nichts geändert hat (`inProgress` ist leer nach dem Cleanup). Da `WriteMetadata` ein Stub ist, hat das aktuell keine Auswirkung — sobald der Stub implementiert wird, schreibt der Manager bei jedem Ticker-Tick im Leerlauf.

**Fix:** Bedingung auf `hasChanged` reduzieren.

---

#### [IMPL-10] Entwicklungs-Logging ist hardcoded aktiv
`cmd/main.go` Zeile 149: `Development: true` ist fest gesetzt und nicht konfigurierbar. Im Development-Mode werden Stack-Traces bei jedem Error-Log ausgegeben, was die Logs in Produktion erheblich aufbläht.

**Fix:** `Development: false` als Default; über Flag oder Umgebungsvariable konfigurierbar machen.

---

### 🟢 NIEDRIG — Code-Qualität

#### [QUAL-01] `MirrorClient.RC` ist ein exported Field — kaputte Kapselung
`pkg/mirror/client/client.go`: Das `regclient.RegClient`-Feld ist als `RC` exportiert. Externe Packages können die interne Implementierung direkt umgehen.

**Fix:** Feld auf `rc` (unexported) umbenennen und bei Bedarf eine Interface-Methode ergänzen.

---

#### [QUAL-02] Fehlende Validierungs-Webhooks für CRDs
Weder `ImageSet` noch `MirrorTarget` haben Admission-Webhooks zur Eingabevalidierung. Ungültige Registry-URLs, leere `targetRef`-Felder oder widersprüchliche `MinVersion`/`MaxVersion`-Angaben werden erst zur Laufzeit als Fehler sichtbar.

**Fix:** `ValidatingAdmissionWebhook` für beide CRDs implementieren.

---

#### [QUAL-03] Fehlende SecurityContext auf Manager- und Worker-Pods
`internal/controller/mirrortarget_controller.go`: Der dynamisch erstellte Manager-Pod hat keinen `securityContext` (kein `runAsNonRoot`, kein `allowPrivilegeEscalation: false`, keine `capabilities`-Einschränkungen). Das `controller-manager`-Deployment in `config/manager/manager.yaml` hat diese korrekt gesetzt — der Manager-Pod nicht.

`pkg/mirror/manager/manager.go` → `startWorker()`: Worker-Pods werden ebenfalls ohne Security Context erstellt. Cluster-weite Pod Security Admission (PSA) im `restricted`-Profil wird Worker-Pods ablehnen und zu unerklärlichen Fehlern führen.

**Fix:** Security-Context analog zu `manager.yaml` auf beide anwenden: `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `capabilities: drop: ["ALL"]`, `seccompProfile: RuntimeDefault`.

---

#### [QUAL-04] Test-Assertions in `imageset_controller_test.go` sind unvollständig
Die Reconciler-Tests (beide Controller) enthalten ausschließlich `TODO`-Kommentare für spezifische Assertions und validieren nur, dass kein Error auftritt. Das Verhalten der Controller (Status-Updates, Deployment-Erstellung) wird nicht geprüft.

**Fix:** Konkrete Status-Assertions hinzufügen; z.B. prüfen, dass nach Reconcile ein Deployment existiert und `MirrorTarget.Status.Conditions` korrekt gesetzt ist.

---

---

## Verifikation: Stand der Umsetzung (2026-04-17)

### ✅ Behoben

| ID | Finding | Nachweis |
|----|---------|---------|
| **IMPL-01** | Compile-Fehler in `cmd/main.go` | Imports (`context`, `fmt`, `mirrorclient`) korrekt ergänzt |
| **SEC-01** | Auth-Credentials nicht an Worker übergeben | `startWorker()` mountet `mt.Spec.AuthSecret` als Volume; `DOCKER_CONFIG` Env-Var gesetzt |
| **SEC-02** | Log-Injection + unzuverlässige Tier-Kommunikation | Log-Parsing vollständig ersetzt durch HTTP-Status-API (`/status` Endpoint); Mutex für `inProgress` |
| **SEC-03** | Manager-Pod nutzt Controller-SA | Dedizierter SA `oc-mirror-coordinator` für Manager, `oc-mirror-worker` für Worker |
| **SEC-05** | HTTP-Client ohne Timeout | `http.Client{Timeout: 30 * time.Second}` in `release.go` |
| **IMPL-06** | Fehler in `release.go` ignoriert | Alle 3 Fehler (`url.Parse`, `NewRequestWithContext`, `io.ReadAll`) korrekt behandelt |
| **IMPL-09** | `hasChanged`-Bedingung fehlerhaft | Korrigiert auf `if hasChanged` (ohne `|| len(m.inProgress) == 0`) |
| **ARCH-06** | In-Memory State nicht thread-safe | `sync.RWMutex` für `inProgress`-Map ergänzt |
| **QUAL-03** | Fehlende SecurityContext auf Pods | Beide Pods (Manager + Worker) erhalten jetzt `runAsNonRoot`, `allowPrivilegeEscalation: false`, `capabilities: drop: ALL`, `seccompProfile: RuntimeDefault` |

---

### 🟡 Teilweise behoben — Compile-Fehler vorhanden

| ID | Finding | Status |
|----|---------|--------|
| **IMPL-02** | State Management ist Stub | Echte `regclient`-Implementierung begonnen, aber **2 Compile-Fehler** blockieren den Build: `BlobGet` falsche Argument-Anzahl (braucht `descriptor.Descriptor`); `manifest.New` / `manifest.Config` / `manifest.Layer` nicht in der regclient-API. |
| **IMPL-03** | CatalogResolver ist Stub | `ExtractImages()`-Helper und `FilterFBC`-Verbesserung hinzugefügt. `ResolveCatalog` gibt aber noch immer nur das Catalog-Image zurück (FBC-Layer-Pull fehlt). Zusätzlich: **Compile-Fehler** `rRef declared and not used` (Zeile 25). |
| **IMPL-04** | Alle Releases/Operator gleiche Destination | Release-Destinations verbessert (Version+Tag-Suffix). Operator-Destinations nutzen aber weiterhin nur den Tag des Catalog-Images, nicht den Catalog-Namen → Mehrere Kataloge überschreiben sich noch immer. |

---

### ❌ Nicht behoben

| ID | Kategorie | Finding |
|----|-----------|---------|
| **SEC-04** | Security | `CONTROLLER_IMAGE` ohne Validierung; Fallback auf `controller:latest` bleibt |
| **SEC-06** | Security | `role.yaml` ist weiterhin `ClusterRole` statt `Role` mit Namespace-Binding |
| **ARCH-01** | Architektur | Manager nutzt weiterhin 30s-Polling-Loop statt event-driven Controller |
| **ARCH-02** | Architektur | `WorkerPool` in `worker.go` ist weiterhin Dead Code (nirgendwo genutzt) |
| **ARCH-03** | Architektur | Dualer Write-Pfad für ImageSet-Status (Controller + Manager) bleibt bestehen |
| **ARCH-04** | Architektur | Keine Finalizer-Implementierung für Cleanup beim Löschen |
| **ARCH-05** | Architektur | `ImageSetReconciler` re-collected nicht bei Spec-Änderungen |
| **ARCH-06** | Architektur | `inProgress`-Map ist thread-safe (Mutex), aber weiterhin nicht persistent (Verlust bei Pod-Restart) |
| **ARCH-07** | Architektur | Lineare Iteration über alle ImageSets bleibt; keine Indexer-Nutzung |
| **IMPL-05** | Implementierung | Worker-Pods haben keine `ownerReference` → Waisen bei Manager-Neustart |
| **IMPL-07** | Implementierung | Release-Resolver ignoriert `MinVersion`, `ShortestPath`, `Full` |
| **IMPL-08** | Implementierung | Kein Retry-Mechanismus für fehlgeschlagene Images |
| **IMPL-10** | Implementierung | `Development: true` in Logging hardcoded |
| **IMPL-11** | Implementierung | Fehler werden nicht in CRD-Conditions (`Status.Conditions`) sichtbar gemacht |
| **IMPL-12** | Implementierung | `go.mod` enthält ungültige Go-Version `1.25.7` |
| **QUAL-01** | Qualität | `MirrorClient.RC` bleibt exported |
| **QUAL-02** | Qualität | Keine Validierungs-Webhooks für CRDs |
| **QUAL-04** | Qualität | Test-Assertions weiterhin unvollständig (nur `Expect(err).NotTo(HaveOccurred())`) |

---

### 🆕 Neu eingeführte Findings durch die Änderungen

| ID | Beschreibung |
|----|-------------|
| **NEW-01** | **HTTP Status-API ohne Authentifizierung**: Der neue `/status`-Endpoint in `manager.go` (Port 8080) akzeptiert POST-Requests ohne jegliche Authentifizierung. Jeder Pod im Namespace kann gefälschte Digest-Werte und Status-Updates einsenden. Dies ist ein direkter Nachfolger des behobenen SEC-02 — die Log-Injection wurde durch eine unauthentifizierte API ersetzt. **Fix:** Mutual TLS oder Bearer-Token-Authentifizierung für den Status-Endpoint. |
| **NEW-02** | **Compile-Fehler in `state.go` (IMPL-02-Versuch)**: `BlobGet` braucht 3 Argumente (`ctx`, `ref`, `descriptor.Descriptor`); `manifest.New` / `manifest.Config` / `manifest.Layer` sind keine öffentlichen Typen der regclient-API in dieser Form. |
| **NEW-03** | **Compile-Fehler in `catalog/resolver.go` (IMPL-03-Versuch)**: Variable `rRef` (Zeile 25) wird deklariert aber nie genutzt. |

---

## Zusammenfassung Umsetzungsstand

| Kategorie | Gesamt | ✅ Behoben | 🟡 Teilweise | ❌ Offen | 🆕 Neu |
|-----------|--------|-----------|-------------|---------|--------|
| Implementierung | 12 | 3 | 2 | 5+2\* | 2 |
| Security | 6 | 3 | — | 2 | 1 |
| Architektur | 7 | — | 1 | 6 | — |
| Qualität | 4 | 1 | — | 3 | — |
| **Gesamt** | **29** | **7** | **3** | **16** | **3** |

\* inkl. der neu eingeführten Compile-Fehler NEW-02/NEW-03

> ⚠️ **Aktueller Build-Status: BROKEN** — 6 Compile-Fehler verhindern `go build ./...`



| Kategorie   | Kritisch | Hoch | Mittel | Niedrig |
|-------------|----------|------|--------|---------|
| Implementierung | 3 | — | 9 | 1 |
| Security    | — | 5 | — | 1 |
| Architektur | — | — | 7 | — |
| Qualität    | — | — | — | 4 |
| **Gesamt**  | **3**    | **5** | **16** | **5** |

> Quellen: Eigenes Review + Gemini-Review (`tasks-gemini.md`) konsolidiert am 2026-04-17.

**Prioritäten:**
1. `IMPL-01` beheben → Projekt muss kompilierbar sein
2. `IMPL-02` + `IMPL-03` → Kernfunktionalität (State, Catalog) implementieren
3. `SEC-01` → Auth-Credentials an Worker übergeben
4. `SEC-02` → Log-Injection-Vektor entfernen + robuste Tier-Kommunikation
5. `ARCH-03` + `ARCH-05` + `ARCH-06` → Duale Write-Pfade, fehlende Re-Sync-Logik, In-Memory-State absichern
6. `QUAL-03` → SecurityContext auf Manager- und Worker-Pods
