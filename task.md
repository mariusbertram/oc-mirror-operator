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
