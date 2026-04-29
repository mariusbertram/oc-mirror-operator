# Plan: State-Konsolidierung, Release-Signaturen & Resource API Refactoring

## Problembeschreibung

### Issue 1: Per-ImageSet State verursacht Doppelzählung und unsicheres Löschen
- Image State wird pro ImageSet in separaten ConfigMaps gespeichert (`<imageset>-images`)
- Shared Images (z.B. Confluent Operator in Catalog 4.20 + 4.21) werden mehrfach gezählt
- `destToIS map[string]string` kann nur ein ImageSet pro Destination speichern → Status-Updates gehen an das falsche ImageSet
- Cleanup löscht Images aus dem Registry ohne zu prüfen, ob ein anderes ImageSet sie noch benötigt

### Issue 2: Release-Signaturen nicht über API verfügbar
- `ReplicateSignature()` ist ein Stub (`ErrNotImplemented`)
- Kein Code lädt Signaturen von `mirror.openshift.com/pub/openshift-v4/signatures/`
- `<imageset>-signatures` ConfigMap wird nie erstellt
- API-Endpoint gibt immer "No release signatures available yet" zurück

### Issue 3: Resource API Refactoring
- Resource Server läuft aktuell embedded im Manager Pod (pro MirrorTarget)
- Soll als separater Pod laufen, aggregiert über **alle** MirrorTargets
- JSON-Daten sollen in ConfigMaps vorgehalten werden (Manager schreibt, API liest)
- Geringerer Memory-Footprint (kein In-Memory-Rendering pro Request)
- Simple Web UI zum Anzeigen der Daten

---

## Architektur-Entscheidungen

### State-Modell: Per-Ref Metadata statt flaches `ImageSets []string`

Ein `ImageEntry` bekommt ein `Refs`-Feld mit per-ImageSet Metadaten. Das löst das Problem, dass bei shared Images EntrySig/OriginRef/Origin pro ImageSet unterschiedlich sein können:

```go
type ImageRef struct {
    ImageSet  string      `json:"imageSet"`
    Origin    ImageOrigin `json:"origin,omitempty"`
    EntrySig  string      `json:"entrySig,omitempty"`
    OriginRef string      `json:"originRef,omitempty"`
}

type ImageEntry struct {
    Source            string     `json:"source"`
    State             string     `json:"state"`
    LastError         string     `json:"lastError,omitempty"`
    RetryCount        int        `json:"retryCount,omitempty"`
    PermanentlyFailed bool       `json:"permanentlyFailed,omitempty"`
    Refs              []ImageRef `json:"refs"` // NEU: welche ImageSets referenzieren dieses Image
}
```

Destination-level State (Mirrored/Pending/Failed) bleibt global — ein Image muss nur einmal gespiegelt werden. Pro-Ref Metadaten ermöglichen korrektes Filtern, Cleanup und Status-Reporting.

### Migration: Zwei-Phasen, kein sofortiges Löschen

1. **Phase**: Alle Reader unterstützen both old (per-IS) und new (per-MT) Format
2. **Phase**: Manager schreibt/pflegt neue konsolidierte ConfigMap; alte werden nach erfolgreicher Migration als "migrated" annotiert und erst nach N Reconcile-Zyklen gelöscht

### Cleanup: Immutable Snapshots

Cleanup-Jobs arbeiten nicht auf der Live-ConfigMap, sondern auf einem bei Job-Erstellung erzeugten Snapshot (separate ConfigMap mit den zu löschenden Destinations). Das verhindert Races bei gleichzeitigem Re-Add.

### ConfigMap-Größe

Gzip-Komprimierung gibt ~30 Bytes/Image. Mit `Refs`-Feld ~50 Bytes/Image. Bei 20k unique Images → ~1 MB. Grenzwertig.

**Mitigation**: Size-Guard im Save(). Falls >900KB komprimiert: Split auf `<mt>-images-0`, `<mt>-images-1` etc. (Sharding by hash prefix des Destination-Key). Wird als optionale Erweiterung im Code vorbereitet, aber erst bei Bedarf aktiviert.

### Resource API: ConfigMap-basierte Datenhaltung + separater Pod

**Aktuell**: Resource Server läuft als Goroutine im Manager Pod, berechnet IDMS/ITMS/etc. on-the-fly per Request, braucht Registry-Zugriff für Catalog-Packages.

**Neu**:
- Manager schreibt vorgerenderte Daten in Resource-ConfigMaps: `<mt>-resource-idms`, `<mt>-resource-itms`, `<mt>-resource-catalogs`, `<mt>-resource-signatures`, `<mt>-resource-index`
- Resource API Pod (1 Replica Deployment, deployed vom Operator Controller) liest diese ConfigMaps
- Resource API Pod aggregiert über ALLE MirrorTargets im Namespace
- Kein Registry-Zugriff im API Pod nötig (Manager resolved Catalog-Packages)
- Simple Web UI als embedded static assets (HTML/CSS/JS, kein Framework)

**API-Struktur (neu)**:
```
GET /                                           → Web UI
GET /api/v1/targets                             → Liste aller MirrorTargets mit Status
GET /api/v1/targets/{target}                    → Detail eines MirrorTargets
GET /api/v1/targets/{target}/imagesets          → ImageSets eines Targets
GET /api/v1/targets/{target}/imagesets/{is}      → Detail ImageSet
GET /api/v1/targets/{target}/imagesets/{is}/idms.yaml
GET /api/v1/targets/{target}/imagesets/{is}/itms.yaml
GET /api/v1/targets/{target}/imagesets/{is}/signature-configmaps.yaml
GET /api/v1/targets/{target}/imagesets/{is}/catalogs/{cat}/catalogsource.yaml
GET /api/v1/targets/{target}/imagesets/{is}/catalogs/{cat}/clustercatalog.yaml
GET /api/v1/targets/{target}/imagesets/{is}/catalogs/{cat}/packages.json
```

**Web UI**: Zeigt Dashboard mit allen MirrorTargets, pro Target die ImageSets, Image-Counts (Total/Mirrored/Pending/Failed), verfügbare Resources zum Download. Single-Page, plain HTML + CSS + fetch()-basiert.

---

## Implementierungsplan

### Phase 1: ImageEntry-Struktur erweitern
**Dateien**: `pkg/mirror/imagestate/imagestate.go`

- `ImageRef` Struct hinzufügen
- `Refs []ImageRef` Feld zu `ImageEntry` hinzufügen
- Alte Felder (`Origin`, `EntrySig`, `OriginRef`) bleiben für Backward-Kompatibilität, werden bei Migration in `Refs` überführt
- Helper-Funktionen:
  - `(e *ImageEntry) HasImageSet(name string) bool`
  - `(e *ImageEntry) AddRef(ref ImageRef)` (dedupliziert)
  - `(e *ImageEntry) RemoveImageSet(name string)` (entfernt Ref, gibt true zurück wenn Refs leer)
  - `(e *ImageEntry) ImageSetNames() []string`
- `ConfigMapNameForTarget(mtName string) string` → `<mtname>-images`
- `Counts()` bleibt — zählt unique Destinations
- `CountsForImageSet(state ImageState, isName string)` → filtert nach Refs
- Neue Load/Save Funktionen für MirrorTarget-Level

### Phase 2: Manager State konsolidieren
**Dateien**: `pkg/mirror/manager/manager.go`, `manager_resolve.go`

- `imageStates map[string]imagestate.ImageState` → `imageState imagestate.ImageState` (eine Map)
- `destToIS map[string]string` → entfernen (Info steckt in `entry.Refs`)
- `dirtyStateNames map[string]bool` → `stateDirty bool`
- `reconcile()` Loop umstrukturieren:
  1. Konsolidierte State einmal laden
  2. Pro ImageSet resolven → Entries mit Ref-Metadata in State mergen
  3. Worker dispatchen aus einer Queue (nicht pro IS)
  4. State einmal speichern
- `setImageStateLocked()`: Update State direkt, kein IS-Lookup nötig
- `updateImageSetStatusLocked()` → wird zu `updateStatusLocked()` — schreibt MirrorTarget Status + pro-IS Status gefiltert

### Phase 3: Migration alter ConfigMaps
**Dateien**: `pkg/mirror/manager/manager.go` (startup), `pkg/mirror/imagestate/imagestate.go`

- Bei Manager-Start: Prüfe ob alte per-IS ConfigMaps existieren
- Merge in neue per-MT ConfigMap (alte Entries: Origin/EntrySig/OriginRef → Refs konvertieren)
- Alte ConfigMaps mit Annotation `mirror.openshift.io/migrated=true` markieren
- Nach 3 erfolgreichen Reconcile-Zyklen: Alte ConfigMaps löschen

### Phase 4: Status-Berechnung fixen
**Dateien**: `internal/controller/mirrortarget_controller.go`, `pkg/mirror/manager/manager.go`

- `aggregateImageSetStatus()`: Lade konsolidierte State, zähle unique Destinations für MT-Level Counts
- Pro-IS Counts: `CountsForImageSet(state, isName)` — filtert nach `entry.Refs`
- ImageSet.Status wird weiterhin vom Manager geschrieben (gefiltert aus konsolidierter State)

### Phase 5: Cleanup fixen
**Dateien**: `internal/controller/mirrortarget_controller.go`, `cmd/main.go`

- Controller: Bei ImageSet-Entfernung
  1. Lade konsolidierte State
  2. Identifiziere Entries wo nur das entfernte IS in `Refs` steht → diese müssen aus Registry gelöscht werden
  3. Erstelle Cleanup-Snapshot-ConfigMap `<mt>-cleanup-<is>` mit Liste der zu löschenden Destinations
  4. Erstelle Cleanup Job mit `--snapshot <configmap-name>`
  5. Entries wo IS entfernt wird aber andere Refs bleiben: nur Ref entfernen, nicht aus Registry löschen
- `runCleanup()`: Liest Snapshot-ConfigMap, löscht nur gelistete Destinations
- Nach erfolgreichem Cleanup: Snapshot-ConfigMap löschen

### Phase 6: Release-Signaturen implementieren
**Dateien**: `pkg/release/signature.go`, `pkg/mirror/manager/manager_resolve.go`

- `SignatureClient.DownloadSignature(ctx, releaseDigest string) ([]byte, error)`:
  - GET `https://mirror.openshift.com/pub/openshift-v4/signatures/openshift/release/<digest-algo>=<hash>/signature-1`
  - Returns raw GPG signature bytes
- Im Manager nach Release-Resolution: Pro resolved Node → Signature downloaden
- Signaturen in `<mt>-signatures` ConfigMap speichern (BinaryData: `sha256-<hash>` → bytes)
- Resource Server (bzw. später Resource API Pod) liest diese ConfigMap

### Phase 7: Resource API als separater Pod
**Dateien**: Neuer Code in `cmd/resource-api/`, `pkg/resourceapi/`, `internal/controller/mirrortarget_controller.go`

**7a: Manager schreibt Resource-ConfigMaps**
- Nach jedem Reconcile-Zyklus: Manager rendert IDMS/ITMS/CatalogSource/etc.
- Schreibt in ConfigMaps:
  - `<mt>-resource-index` → JSON Index aller verfügbaren Resources
  - `<mt>-resource-<is>-idms` → YAML
  - `<mt>-resource-<is>-itms` → YAML
  - `<mt>-resource-<is>-signatures` → YAML
  - `<mt>-resource-<is>-<catalog>-catalogsource` → YAML
  - `<mt>-resource-<is>-<catalog>-clustercatalog` → YAML
  - `<mt>-resource-<is>-<catalog>-packages` → JSON
- ConfigMaps mit Label `app=oc-mirror-resources, mirrortarget=<mt>`
- Nur schreiben wenn Inhalt sich geändert hat (Hash-Check)

**7b: Catalog-Packages im Manager resolven**
- `handleCatalogPackages` / `handleUpstreamCatalogPackages` aktuell laden FBC on-the-fly
- Manager resolved Packages bei Catalog-Build und schreibt Ergebnis in Resource-ConfigMap
- Upstream-Packages: Bei Poll-Intervall aktualisieren

**7c: Resource API Binary**
- Neues Entrypoint: `cmd/resource-api/main.go`
- Liest alle Resource-ConfigMaps im Namespace (per Label-Selector)
- Aggregiert über alle MirrorTargets
- Serviert REST API + Static Web UI
- Watch auf ConfigMaps für Live-Updates (informer/watch)
- Kein Registry-Zugriff, kein Auth-Config nötig

**7d: Operator deployt Resource API**
- `MirrorTargetReconciler`: Statt Resource-Service auf Manager-Pod → erstellt separates Deployment `oc-mirror-resource-api` (1 Replica)
- Einmal pro Namespace (nicht pro MirrorTarget) — idempotent
- Service + Route/Ingress zeigt auf Resource API Pod
- RBAC: Read-only auf ConfigMaps/MirrorTargets/ImageSets im Namespace

**7e: Resource Server aus Manager entfernen**
- Manager startet keinen `:8081` Server mehr
- Port 8081 Service wird auf Resource API Pod umgeleitet
- Manager Pod braucht nur noch Port 8080 (Status API für Worker)

### Phase 8: Web UI
**Dateien**: `pkg/resourceapi/ui/` (embedded static files)

- Single-Page Dashboard, eingebettet via Go `embed`
- Plain HTML + CSS + Vanilla JS (kein Framework)
- Seiten:
  - **Dashboard**: Alle MirrorTargets mit Fortschrittsbalken (Total/Mirrored/Pending/Failed)
  - **Target Detail**: ImageSets mit Status, verfügbare Resources als Download-Links
  - **ImageSet Detail**: Image-Liste mit Filter (Status, Origin), Catalog-Packages
- Daten via `fetch()` gegen `/api/v1/...` Endpoints
- Auto-Refresh alle 30s

### Phase 9: Tests aktualisieren
- `pkg/mirror/imagestate/imagestate_test.go` — neue Struct, Refs, CountsForImageSet
- `pkg/mirror/manager/manager_test.go` — konsolidierter State, kein destToIS
- `internal/controller/mirrortarget_controller_test.go` — Cleanup mit Snapshots
- `internal/controller/mirrortarget_aggregate_test.go` — Deduplizierte Counts
- `internal/controller/controller_coverage_test.go` — Cleanup Coverage
- `pkg/mirror/resources/server_test.go` → `pkg/resourceapi/` Tests
- `pkg/release/signature_test.go` — echte Implementation testen
- Neue Tests: Resource-ConfigMap Write, Migration, Web UI Endpoints
- E2E: Shared-Image Szenario (2 ImageSets mit gleichem Operator)

### Phase 10: Aufräumen
- Alte `pkg/mirror/resources/server.go` entfernen (durch `pkg/resourceapi/` ersetzt)
- ExposeConfig in MirrorTarget: Bleibt, zeigt aber auf Resource API Service
- Deprecation-Kommentare für alte per-IS State Funktionen
- `copilot-instructions.md` und Doku aktualisieren

### Phase 11: E2E Tests anpassen
**Dateien**: `test/e2e/`, `test/e2e_flow/`

**11a: Resource API Deployment-Verifizierung (e2e_mirror_test.go)**
- Nach erfolgreichem Mirroring: Prüfen, dass `oc-mirror-resource-api` Deployment im Namespace existiert und Ready ist
- Prüfen, dass der zugehörige Service `<mt>-resources` existiert und auf Port 8081 konfiguriert ist
- Resource-API Pod-Logs in Diagnostik-Dump aufnehmen (Label `app=oc-mirror-resource-api`)

**11b: Resource-ConfigMap Verifizierung (e2e_mirror_test.go)**
- Nach Mirroring: Prüfen, dass Resource-ConfigMap `oc-mirror-<mt>-resources` existiert
- ConfigMap muss Keys für `idms.yaml` und `index.json` enthalten
- IDMS-Inhalt muss `ImageDigestMirrorSet` YAML mit korrekter Source/Mirror-Zuordnung enthalten

**11c: Catalog Resource-ConfigMap (e2e_catalog_cluster_test.go)**
- Nach CatalogBuildJob Erfolg: Prüfen, dass Resource-ConfigMap CatalogSource- und Packages-Einträge enthält
- Packages-JSON muss mindestens das gefilterte Paket (`ip-rule-operator`) enthalten

**11d: Resource API Endpoint-Tests (e2e_mirror_test.go)**
- Port-Forward zum Resource-API Service, HTTP GET auf `/api/v1/targets`
- Antwort muss den MirrorTarget mit korrektem Status enthalten
- HTTP GET auf `/api/v1/targets/<mt>/imagesets/<is>/idms.yaml` muss gültiges YAML liefern
- Web UI unter `/ui/` muss HTTP 200 zurückgeben

**11e: Lifecycle-Test aktualisieren (e2e_flow/lifecycle_test.go)**
- Resource-ConfigMap-Erstellung nach Manager-Reconcile verifizieren
- Prüfen, dass Manager-Reconcile keine `:8081`-bezogenen Ressourcen mehr erstellt

### Phase 12: README und Dokumentation aktualisieren
**Dateien**: `README.md`, `docs/user-guide.md`, `docs/contributing.md`, `docs/api-reference.md`

**12a: README.md**
- Architektur-Diagramm aktualisieren: Resource API als separates Deployment statt integriert im Manager
- Feature-Tabelle: "Resource Server" → "Resource API + Web UI Dashboard"
- Architektur-Abschnitt: Vier-Tier-Modell (Operator, Manager, Worker, Resource API)
- "Resource Server (HTTP-API)"-Abschnitt komplett ersetzen:
  - Neue Endpoint-Tabelle mit `/api/v1/...` Pfaden
  - ConfigMap-basierte Architektur beschreiben
  - Web UI Dashboard erwähnen (`/ui/`)
  - Beispiele mit neuen URLs aktualisieren
- Quick Start curl-Beispiele anpassen

**12b: docs/user-guide.md**
- Section 10 "Resource Server" komplett überarbeiten:
  - Eigenständiges Deployment statt Manager-integriert
  - Neue API-Pfade (`/api/v1/targets/{mt}/imagesets/{is}/...`)
  - Web UI Dashboard Zugang beschreiben
  - ConfigMap-basierte Datenhaltung erklären
- Section 7.4 "Expose": Service zeigt auf Resource-API Deployment, nicht Manager-Pod
- Manager-Beschreibung: Port 8081 entfernen, nur noch Port 8080 (Worker-Status API)

**12c: docs/contributing.md**
- Paketstruktur aktualisieren: `pkg/mirror/resources/server.go` → `pkg/resourceapi/`
- Neues Paket `pkg/resourceapi/` in der Übersicht aufnehmen
- E2E-Testbeschreibung um Resource-API Tests erweitern

**12d: docs/api-reference.md**
- Neue REST-API Endpoints dokumentieren (`/api/v1/targets`, `/api/v1/targets/{mt}`)
- Web UI Endpoint (`/ui/`) aufnehmen
- Redirect-Kompatibilität für alte `/resources/` Pfade dokumentieren

---

## Abhängigkeiten

```
Phase 1 (ImageEntry erweitern)
  ↓
Phase 2 (Manager konsolidieren) ←── Phase 3 (Migration)
  ↓
Phase 4 (Status fix) + Phase 5 (Cleanup fix)   [parallel]
  ↓
Phase 6 (Signaturen)   [unabhängig, kann parallel zu 4/5]
  ↓
Phase 7 (Resource API Pod + ConfigMap-basiert)
  ↓
Phase 8 (Web UI)
  ↓
Phase 9 (Tests)   [inkrementell mit jeder Phase]
  ↓
Phase 10 (Aufräumen)
  ↓
Phase 11 (E2E Tests anpassen)
  ↓
Phase 12 (README + Docs)
```

## Risiken

- **ConfigMap-Größe**: Konsolidierung könnte bei >15k Images die 1MB-Grenze erreichen. Mitigation: Size-Guard + Sharding vorbereiten.
- **Migration**: Bestandsinstallationen haben per-IS ConfigMaps. Zweiphasen-Migration reduziert Risiko.
- **Resource API Memory**: ConfigMap-Watch über viele MirrorTargets. Mitigation: Informer mit Label-Filter.
- **Breaking Change**: Resource API URLs ändern sich (`/resources/{is}/...` → `/api/v1/targets/{mt}/imagesets/{is}/...`). Mitigation: Redirect-Handler für alte URLs in Übergangsphase.
