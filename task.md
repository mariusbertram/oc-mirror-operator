# Aufgabenliste: ocp-mirror Operator

## 1. Projekt-Setup
- [x] Erstelle ein neues GitHub Repository oder forke `openshift/oc-mirror` mit dem `gh` CLI.
- [x] Initialisiere das Go-Projekt mit `operator-sdk init --domain mirror.openshift.io --repo github.com/mariusbertram/ocp-mirror`.
- [x] Integriere `github.com/regclient/regclient` als Go-Dependency.
- [x] Nutze `replace` Direktive für lokale Einbindung der `oc-mirror` CLI-Libraries.

## 2. API / CRD Definitionen
- [x] Erstelle die `MirrorTarget` API.
- [x] Definiere die `spec`-Struktur für `MirrorTarget` (Ziel-Registry-URL, AuthSecret).
- [x] Erstelle die `ImageSet` API.
- [x] Definiere die `spec`-Struktur für `ImageSet`, inkl. Referenz auf ein `MirrorTarget` (`targetRef`) und Mirror-Konfiguration.
- [x] Definiere die `status`-Struktur für `ImageSet` zur Speicherung der Soll-Liste (TargetImages).
- [x] Generiere die CRDs (`make manifests`).

## 3. Core Logic & regclient Integration
- [x] Implementiere den Basis-Wrapper für `regclient` (`pkg/mirror/mirror.go`).
- [x] Entwickle die Funktion zum Spiegeln von Images inkl. Signaturen (`CopyImage`).
- [x] Implementiere die Hintergrund-Existenzprüfung (`CheckExist`).
- [x] Logik für Digest-zu-Tag Mapping integriert.
- [x] Basis Unit-Tests für Mirror-Funktionen erstellt.

## 4. OLM Catalog Filter Engine
- [x] Implementiere Logik zum Herunterladen und Entpacken des Catalogs (via `regclient`).
- [x] Filter-Algorithmus für OLM Packages entwickeln (`pkg/catalog/filter.go`).
- [ ] Re-Build und Push des gefilterten Catalogs (in Arbeit).

## 5. Reconciler & Worker Pool
- [x] Implementiere `MirrorTargetReconciler`.
- [x] Implementiere `ImageSetReconciler` mit Replikations-Loop.
- [x] Basis-Soll-Listen Generierung für `AdditionalImages`.
- [x] Ausbau des Worker-Pools zur parallelen Abarbeitung über mehrere CRs hinweg (`pkg/mirror/worker.go`).

## 6. OpenShift Releases & IDMS/ITMS
- [x] Implementiere Replikation der OpenShift Release-Signaturen (`pkg/release/signature.go`).
- [x] Erstelle Generatoren für `ImageDigestMirrorSet` (IDMS) und `ImageTagMirrorSet` (ITMS) (`pkg/mirror/idms_itms.go`).
- [x] Automatisches Lifecycle-Management für IDMS/ITMS im Cluster (integriert in Reconciler).

## 7. Abschluss & CI/CD
- [x] Vervollständige Unit-Tests für Kernfunktionen.
- [ ] Richte GitHub Actions für Tests und Linter ein.
- [ ] Teste den Operator lokal in einem Kind Cluster.
- [ ] Generiere das OLM Bundle (`make bundle`).
