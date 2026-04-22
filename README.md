# oc-mirror-operator

Der `oc-mirror-operator` ist ein Kubernetes-Operator zur Automatisierung und kontinuierlichen Spiegelung von OpenShift Releases, Operator-Katalogen und zusätzlichen Container-Images in eine private Registry.

Im Gegensatz zum statischen `oc-mirror` CLI-Tool arbeitet dieser Operator cloud-nativ und deklarativ. Er orchestriert Mirroring-Workflows direkt im Cluster — ohne persistenten Storage, ohne externe Abhängigkeiten und mit voller Kubernetes-Integration über Custom Resources.

---

## Feature-Vergleich: oc-mirror-operator vs. oc-mirror CLI

### ✅ Implementierte Features

| Feature | oc-mirror CLI | oc-mirror-operator | Anmerkungen |
|---------|:---:|:---:|-------------|
| **OpenShift Release Mirroring** | ✅ | ✅ | Cincinnati-Graph-Auflösung, Versions-Ranges, BFS Shortest-Path, Full-Channel-Modus |
| **Release Component-Images** | ✅ | ✅ | Automatische Extraktion aller ~190 Component-Images aus dem Release-Payload |
| **Multi-Architektur Support** | ✅ | ✅ | `architectures: [amd64, arm64, ...]` — Multi-Arch-Manifest-Auflösung |
| **OCP und OKD** | ✅ | ✅ | `type: ocp` (Default) oder `type: okd` |
| **Operator-Katalog Mirroring** | ✅ | ✅ | FBC-Parsing, Package-Filterung, Bundle-Image-Extraktion mit automatischer Dependency-Resolution |
| **Operator Dependency Resolution** | ✅ | ✅ | Transitive BFS-Auflösung von `olm.package.required`, `olm.gvk.required` und Companion-Packages (z.B. `odf-operator` → `odf-dependencies`) |
| **Gefiltertes Katalog-Image (OLM v0+v1)** | ✅ | ✅ | Source-Image als Basis + FBC-Overlay-Layer mit Opaque Whiteouts — kompatibel mit CatalogSource (gRPC) und ClusterCatalog |
| **Package/Channel-Filterung** | ✅ | ✅ | Einzelne Packages und Channels selektierbar |
| **Version-Ranges (Operators)** | ✅ | ✅ | `minVersion` / `maxVersion` pro Package oder Channel |
| **Additional Images** | ✅ | ✅ | Einzelne Images mit optionalem `targetRepo` / `targetTag` |
| **Cosign-Signaturen** | ✅ | ✅ | Tag-basierte `.sig` Signaturen werden automatisch mit kopiert |
| **OCI Referrers** | ✅ | ✅ | Attestations, SBOMs über `regclient.ImageWithReferrers()` |
| **Release-Signaturen** | ✅ | ✅ | Download von mirror.openshift.com/pub/openshift-v4/signatures |
| **IDMS/ITMS-Generierung** | ✅ | ✅ | ImageDigestMirrorSet und ImageTagMirrorSet — bereitgestellt via Resource Server HTTP-API |
| **Inkrementelles Mirroring** | ✅ | ✅ | Bereits gespiegelte Images werden übersprungen (ConfigMap-State) |
| **Registry-Drift-Detection** | ✗ | ✅ | Manager verifiziert alle 5 Min. ob gespiegelte Images noch in der Registry existieren; fehlende werden automatisch neu gespiegelt. Auth-Token-Refresh alle 20 Checks verhindert Quay-nginx-8KB-Header-Limit |
| **Per-Image Timeout** | ✗ | ✅ | 20 Min. Timeout pro Image-Mirror verhindert, dass steckengebliebene Uploads den Worker blockieren |
| **Automatische Retries** | ✗ | ✅ | Bis zu 10 Wiederholungen pro Image bei Fehlern |
| **Kontinuierliches Mirroring** | ✗ | ✅ | Reconcile-Loop alle 30s — neue Images werden automatisch erkannt und gespiegelt |
| **Deklarativ via CRDs** | ✗ | ✅ | `MirrorTarget` (Ziel + ImageSet-Liste) + `ImageSet` (Inhalts-Definition) Custom Resources |
| **Skalierbare Worker-Pods** | ✗ | ✅ | Konfigurierbare Concurrency (bis 100 Pods) und BatchSize (bis 100 Images/Pod) |
| **Ephemeral-Volume Blob-Buffering** | ✗ | ✅ | Große Blobs (>100 MiB) werden auf emptyDir gepuffert — kein OOM bei Multi-GB Layern |
| **Blob-Replikationsplanung** | ✗ | ✅ | Greedy-Set-Cover-Algorithmus optimiert die Mirror-Reihenfolge für maximale Blob-Wiederverwendung |
| **Automatischer Catalog-Rebuild** | ✗ | ✅ | Build-Signatur erkennt Änderungen an Packages/Katalogen und triggert automatischen Rebuild |
| **Resource Server (HTTP-API)** | ✗ | ✅ | IDMS, ITMS, CatalogSource, ClusterCatalog und Signatur-ConfigMaps per HTTP abrufbar — mit OpenShift Route, Ingress oder Service |
| **Worker-Pod-Lifecycle** | ✗ | ✅ | Automatische Bereinigung abgeschlossener/fehlgeschlagener Worker- und Orphan-Pods |
| **KubeVirt Container-Disk** | ✅ | ✅ | `platform.kubeVirtContainer: true` extrahiert KubeVirt-Disk-Images aus dem Release-Payload (RHCOS pro Architektur) |

### ⚠️ Teilweise implementiert

| Feature | oc-mirror CLI | oc-mirror-operator | Status |
|---------|:---:|:---:|--------|
| **GatewayAPI-Exposure** | ✗ | ⚠️ | API-Feld definiert (`spec.expose.type: GatewayAPI`), HTTPRoute-Erstellung noch nicht implementiert |

### ❌ Nicht implementierte Features

| Feature | oc-mirror CLI | oc-mirror-operator | Bemerkung |
|---------|:---:|:---:|-----------|
| **Blocked Images** | ✅ | ❌ | API-Feld `blockedImages` existiert, wird aber nirgends ausgewertet |
| **Helm Chart Mirroring** | ✅ | ❌ | Vollständige API-Typen definiert (`Helm`, `Repository`, `Chart`), aber Collector ignoriert `spec.mirror.helm` |
| **Mirror-to-Disk** | ✅ | ❌ | `oc-mirror` kann in ein lokales Archiv spiegeln — kein Äquivalent im Operator (nicht sinnvoll im Cluster-Kontext) |
| **Disk-to-Mirror** | ✅ | ❌ | `oc-mirror` kann von einem lokalen Archiv in eine Registry spiegeln — `platform.release` Feld existiert, wird aber nicht verwendet |
| **Enclave Support** | ✅ | ❌ | Kein Konzept für Air-Gap-Transfer über Datenträger — der Operator benötigt Netzwerkzugang zu Quell- und Ziel-Registry |
| **UpdateService CR** | ✅ | ❌ | `oc-mirror` generiert ein UpdateService CR für OSUS — nicht implementiert |
| **Cincinnati Graph Data** | ✅ | ❌ | `platform.graph: true` Feld existiert, Graph-Daten werden aber nicht in die Ziel-Registry gepusht |
| **Pruning / Image-Bereinigung** | ✅ | ❌ | Kein automatisches Löschen veralteter Images aus der Ziel-Registry |
| **Samples** | ✅ | ❌ | API-Feld existiert, explizit als "not implemented" markiert |

### Operator-spezifische Features (kein Äquivalent in oc-mirror CLI)

| Feature | Beschreibung |
|---------|-------------|
| **Cloud-native Orchestrierung** | Läuft als Kubernetes-Operator im Cluster — kein manuelles CLI-Aufrufen nötig |
| **Worker-Pod-Architektur** | Parallele Worker-Pods mit konfigurierbarer Concurrency und Batch-Größe |
| **Authentifizierte Status-API** | Worker melden Ergebnisse via Bearer-Token-geschütztem HTTP-Endpoint |
| **ConfigMap-basierter State** | Gzip-komprimierter Image-State in ConfigMaps (~30 Bytes/Image) — kein PV nötig |
| **Restricted Pod Security** | Alle Pods laufen mit `runAsNonRoot`, Drop-All-Capabilities, Seccomp |
| **Namespace-scoped RBAC** | Keine ClusterRole — alle Rechte auf den Operator-Namespace begrenzt |
| **Registry-Existenz-Check** | Periodische Prüfung ob Images in der Ziel-Registry noch vorhanden sind |
| **Quay-Kompatibilität** | Spezielles Blob-Buffering für Quay-Registries (Upload-Session-Timeout-Workaround) |
| **Blob-Replikationsplanung** | Greedy-Set-Cover optimiert Mirror-Reihenfolge: Shared-Layer werden zuerst gepusht → Folge-Images nutzen Blob-Mount (Zero-Copy) |
| **Catalog-Build-Signatur** | SHA256-Hash über Operator-Image + Katalog + Package-Liste erkennt automatisch wann ein Rebuild nötig ist |
| **Resource Server (HTTP-API)** | Stellt IDMS/ITMS, CatalogSource, ClusterCatalog und Signatur-ConfigMaps per REST bereit — Route (OpenShift), Ingress oder Service |

---

## Architektur

Die Architektur folgt einem skalierbaren **Drei-Schichten-Modell** mit **Catalog-Build-System** und **Resource Server**:

```
┌─────────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                                 │
│                                                                     │
│  ┌──────────────────┐   watch   ┌──────────────────────────────┐    │
│  │  ImageSet CR     │◄──────────│                              │    │
│  │  MirrorTarget CR │           │   Operator (Control Plane)   │    │
│  └──────────────────┘  reconcile│   internal/controller/       │    │
│                         ───────►│                              │    │
│                                 └──────┬──────────────┬────────┘    │
│                                        │ creates      │ creates     │
│                                        │ Deployment   │ Job         │
│                                        │ Service ×2   │             │
│                                        │ Route/Ingress│             │
│                                        ▼              ▼             │
│                  ┌────────────────────────┐  ┌────────────────────┐ │
│                  │ Manager Pod            │  │ Catalog-Builder    │ │
│                  │ pkg/mirror/manager/    │  │ Job                │ │
│                  │                        │  │ cmd/catalog-builder│ │
│                  │ • Lädt Image-State     │  │                    │ │
│                  │ • Verwaltet Worker-Q   │  │ • Filtert FBC      │ │
│                  │ • HTTP Status-API :8080│  │ • Löst Deps auf    │ │
│                  │ • Resource Server :8081│  │ • Baut OCI-Image   │ │
│                  │ • Registry-Verifikation│  │ • Pusht Catalog    │ │
│                  │ • Worker-Pod-Cleanup   │  │                    │ │
│                  └──────┬────────┬───┬────┘  └────────────────────┘ │
│                         │creates │   │ :8081                        │
│                         │Pods    │   ▼                              │
│                         │     ┌──┴──────────────────────┐           │
│                         │     │ Route / Ingress / Svc   │           │
│                         │     │ → /resources/           │           │
│                         │     │ IDMS, ITMS, CatalogSrc  │           │
│                         │     │ ClusterCatalog, Sigs    │           │
│                         │     └─────────────────────────┘           │
│                         │ receives POST /status                     │
│                  ┌──────▼───────┐                                   │
│                  │ Worker Pod 1 │                                   │
│                  │ Worker Pod 2 │                                   │
│                  │ Worker Pod N │                                   │
│                  └──────┬───────┘                                   │
│                         │ regclient + emptyDir                      │
│                         ▼                                           │
│                  ┌──────────────────────────────┐                   │
│                  │   Ziel-Registry              │                   │
│                  └──────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────────┘
```

### Komponenten

| Schicht | Komponente | Beschreibung |
|---------|------------|-------------|
| **Control Plane** | `Operator` (`internal/controller/`) | Überwacht CRs, berechnet Image-Soll-Listen via Cincinnati-API und FBC-Parsing, erstellt Catalog-Build-Jobs, setzt Status-Conditions |
| **Orchestration** | `Manager` (`pkg/mirror/manager/`) | Ein Deployment pro `MirrorTarget`. Lädt Image-State, plant Mirror-Reihenfolge (Blob-Planner), startet Worker-Pods, empfängt Ergebnisse via authentifizierter HTTP-API (:8080), verifiziert Registry-Zustand, bereinigt abgeschlossene Pods |
| **Resource Server** | `Resource Server` (`pkg/mirror/resources/`) | Integriert im Manager-Pod auf Port :8081. Stellt IDMS, ITMS, CatalogSource, ClusterCatalog und Signatur-ConfigMaps per HTTP-REST bereit. Exponiert via Route (OpenShift), Ingress oder Service |
| **Execution** | `Worker` (kurzlebige Pods) | Kopiert Image-Batches mit `regclient`, puffert große Blobs auf emptyDir, kopiert Signaturen, meldet Status via `POST /status` |
| **Catalog Build** | `Catalog-Builder` (K8s Job) | Pro Quell-Katalog ein Job: filtert FBC, löst Dependencies auf, baut OCI-Image mit Source-Layers + FBC-Overlay, pusht in Ziel-Registry |
| **State** | ConfigMap (gzip-JSON) | Per-Image Mirroring-Status in Kubernetes ConfigMaps — kein PV/PVC nötig, ~30 Bytes pro Image |

### Datenfluss

1. Nutzer erstellt `MirrorTarget` (Ziel-Registry + ImageSet-Liste) und `ImageSet` (Inhalts-Definition) CRs
2. **Operator** löst via Cincinnati-API (Releases) und Catalog-Image (Operators) die vollständige Image-Liste auf und speichert sie als gzip-komprimierte ConfigMap
3. **Operator** erstellt Catalog-Builder-Jobs für jeden konfigurierten Operator-Katalog (mit Build-Signatur zur Änderungserkennung)
4. **Catalog-Builder** filtert FBC, löst Dependencies auf, baut OCI-Image und pusht es in die Ziel-Registry
5. **Manager** lädt den Image-State, plant die Mirror-Reihenfolge via Blob-Planner, prüft ob gespiegelte Images noch in der Ziel-Registry vorhanden sind und startet Worker-Pods
6. **Worker** kopiert Images (inkl. Signaturen und Referrers), puffert große Blobs auf Ephemeral Volume und meldet Ergebnisse via `POST /status` an den Manager
7. Manager bereinigt abgeschlossene/fehlgeschlagene Worker-Pods und aktualisiert Image-State und `ImageSet.Status`
8. **Resource Server** (Port 8081 im Manager-Pod) stellt Cluster-Ressourcen per HTTP bereit — IDMS, ITMS, CatalogSource, ClusterCatalog und Signatur-ConfigMaps werden erst ausgeliefert wenn das jeweilige ImageSet den Status `Ready` hat

---

## Operator-Katalog-System

Der Operator baut gefilterte OCI-Katalog-Images, die mit **OLM v0** (CatalogSource/gRPC) und **OLM v1** (ClusterCatalog) kompatibel sind.

### Dependency Resolution

Beim Filtern eines Operator-Katalogs werden automatisch alle transitiven Dependencies via **BFS-Traversierung** aufgelöst:

| Dependency-Typ | Beschreibung | Beispiel |
|----------------|-------------|---------|
| `olm.package.required` | Direkte Package-Dependencies aus Bundle-Properties | `odf-dependencies` requires `cephcsi-operator` |
| `olm.gvk.required` | GVK-Dependencies (Group/Version/Kind), aufgelöst zum Provider-Package | Bundle benötigt `StorageCluster` API → `ocs-operator` |
| Companion-Packages | Red-Hat-Namenskonvention: `<name>-dependencies`, `<name>-deps` | `odf-operator` → `odf-dependencies` |

### Gefiltertes Katalog-Image (OCI Layer-Architektur)

```
┌──────────────────────────────────────────┐
│  Layer 6: Filtered FBC Overlay (neu)     │  ← configs/<pkg>/catalog.yaml
│           + Opaque Whiteouts             │  ← configs/.wh..wh..opq
│           + Cache-Invalidierung          │  ← tmp/cache/.wh..wh..opq
├──────────────────────────────────────────┤
│  Layer 5: Original FBC (full catalog)    │  ← durch Whiteout überdeckt
│  Layer 4: opm Binary + Tools             │
│  Layer 3: OS Dependencies                │
│  Layer 2: Base OS (RHEL UBI)             │
│  Layer 1: Root Filesystem                │
└──────────────────────────────────────────┘
```

- **Source-Image als Basis**: Alle Original-Layers werden per Blob-Copy übernommen (behält `opm` Binary, Entrypoint, OS)
- **FBC-Overlay**: Neuer Layer mit gefiltertem FBC + OCI Opaque Whiteout (`configs/.wh..wh..opq`) überdeckt den vollen Katalog
- **Cache-Invalidierung**: Opaque Whiteout für `/tmp/cache/` entfernt den vorgebauten `opm`-Cache; `--cache-enforce-integrity=false` im Image-Cmd erlaubt Neuaufbau
- **OLM-Label**: `operators.operatorframework.io.index.configs.v1=/configs` für Kompatibilität mit beiden OLM-Versionen

### Build-Signatur und Automatischer Rebuild

Katalog-Builds werden über eine **Build-Signatur** (SHA256-Hash) verwaltet:
- Eingabe: Operator-Image + Katalog-URLs + Full-Flag + sortierte Package-Namen
- Gespeichert als Annotation: `mirror.openshift.io/catalog-build-sig`
- Bei Signatur-Änderung (neues Package, geänderter Katalog): alter Job wird gelöscht, neuer Build gestartet

### Catalog-Builder Job

Pro Quell-Katalog wird ein Kubernetes Job erstellt:
- Container: Gleich Operator-Image, Entrypoint `/catalog-builder`
- Konfiguration über Umgebungsvariablen: `SOURCE_CATALOG`, `TARGET_REF`, `CATALOG_PACKAGES`
- emptyDir-Volume `/tmp/blob-buffer` für große Layer-Blobs
- Max 3 Retries, 10 Minuten TTL nach Abschluss

---

## Blob-Replikationsplanung

Der Manager optimiert die Mirror-Reihenfolge mittels eines **Greedy-Set-Cover-Algorithmus** (`PlanMirrorOrder`):

1. **Phase 1**: Manifeste aller Images abrufen, Blob-Digests pro Image sammeln
2. **Phase 2**: Blob-Häufigkeit zählen (wie viele Images referenzieren jeden Blob)
3. **Phase 3**: Greedy-Sortierung:
   - **Erstes Image**: Dasjenige dessen Blobs in den meisten anderen Images vorkommen (Shared-Layer werden zuerst gepusht)
   - **Folge-Images**: Bevorzugt Images mit den meisten bereits hochgeladenen Blobs (maximiert Blob-Mount-Treffer)

**Effekt**: Blobs die von einem früheren Image gepusht wurden, werden von `regclient` via Anonymous-Blob-Mount (Zero-Copy) verlinkt — kein erneuter Daten-Transfer nötig.

---

## KubeVirt Container-Disk-Images

Wenn `platform.kubeVirtContainer: true` gesetzt ist, extrahiert der Operator die **KubeVirt Container-Disk-Images** (RHCOS-basiert) aus dem Release-Payload und spiegelt sie automatisch mit.

### Funktionsweise

1. Der Collector liest den `0000_50_installer_coreos-bootimages` ConfigMap aus dem Release-Payload
2. Aus dem eingebetteten CoreOS-Stream-JSON werden die `kubevirt.digest-ref` Einträge pro Architektur extrahiert
3. Die Images werden wie reguläre Component-Images in die Ziel-Registry gespiegelt

### Architektur-Mapping

| ImageSet `architectures` | CoreOS Stream Architektur | KubeVirt verfügbar |
|--------------------------|--------------------------|-------------------|
| `amd64` | `x86_64` | ✅ |
| `s390x` | `s390x` | ✅ |
| `arm64` | `aarch64` | ❌ (nicht in allen Releases) |
| `ppc64le` | `ppc64le` | ❌ (nicht in allen Releases) |

> **Hinweis**: Nicht alle Architekturen haben KubeVirt-Images. Fehlende Architekturen werden übersprungen, nicht als Fehler gemeldet.

---

## Resource Server (HTTP-API)

Der Manager-Pod hostet auf Port **8081** einen HTTP-Server, der Cluster-Ressourcen im YAML-Format bereitstellt. Diese Ressourcen können direkt mit `kubectl apply` oder via GitOps auf den Cluster angewendet werden.

### Endpoints

| Endpoint | Ressource | Beschreibung |
|----------|-----------|-------------|
| `GET /resources/` | JSON-Index | Übersicht aller ImageSets mit ihren verfügbaren Ressourcen und Katalogen |
| `GET /resources/{imageset}/idms.yaml` | `ImageDigestMirrorSet` | Digest-basierte Mirror-Regeln für alle gespiegelten Images |
| `GET /resources/{imageset}/itms.yaml` | `ImageTagMirrorSet` | Tag-basierte Mirror-Regeln (falls tag-basierte Images vorhanden) |
| `GET /resources/{imageset}/catalogs/{name}/catalogsource.yaml` | `CatalogSource` | OLM v0-kompatible CatalogSource (gRPC) für den gefilterten Katalog |
| `GET /resources/{imageset}/catalogs/{name}/clustercatalog.yaml` | `ClusterCatalog` | OLM v1-kompatible ClusterCatalog-Ressource |
| `GET /resources/{imageset}/signature-configmaps.yaml` | ConfigMaps | Release-Signatur-ConfigMaps im OpenShift-Verifikationsformat |

### Ready-Gating

Ressourcen werden erst ausgeliefert, wenn das zugehörige ImageSet den Status `Ready` hat. Anfragen vor Abschluss des Mirrorings erhalten HTTP `409 Conflict`. Der JSON-Index (`/resources/`) zeigt für jedes ImageSet den `ready`-Status an.

### Exposure-Optionen

Die externe Erreichbarkeit wird über `MirrorTarget.spec.expose` konfiguriert:

| Typ | Beschreibung | Automatik |
|-----|-------------|-----------|
| **Route** (default auf OpenShift) | OpenShift Route mit Edge-TLS-Terminierung | Auto-Erkennung via Route-API-Discovery |
| **Ingress** | `networking.k8s.io/v1` Ingress | Erfordert `host` und optional `ingressClassName` |
| **GatewayAPI** | Gateway-API HTTPRoute | API-Feld definiert, Implementation ausstehend |
| **Service** (default auf K8s) | Nur ClusterIP-Service, keine externe Exposition | Fallback wenn keine Route-API verfügbar |

Beim Wechsel des Exposure-Typs (z.B. Route → Ingress) werden veraltete Objekte automatisch bereinigt.

### Beispiel-Nutzung

```bash
# Index abrufen
curl -sk https://<route-host>/resources/ | jq .

# IDMS direkt anwenden
curl -sk https://<route-host>/resources/ocp-release-4-21/idms.yaml | kubectl apply -f -

# CatalogSource für gefilterten Operator-Katalog
curl -sk https://<route-host>/resources/ocp-release-4-21/catalogs/redhat-operator-index/catalogsource.yaml | kubectl apply -f -
```

---

## Custom Resources

### MirrorTarget

Definiert das Spiegelungsziel und die Pod-Konfiguration.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
  namespace: oc-mirror-system
spec:
  # Ziel-Registry inkl. Basis-Pfad (Pflicht)
  registry: "registry.example.com/mirror"

  # Liste der ImageSets, die in dieses Target gespiegelt werden (Pflicht)
  # Jedes ImageSet darf nur in einem MirrorTarget referenziert werden.
  imageSets:
    - ocp-4-21-sync
    - additional-tools

  # Referenz auf ein Secret mit Registry-Credentials (empfohlen)
  authSecret: "target-registry-creds"

  # Für Registries mit self-signed Zertifikaten
  insecure: false

  # Parallelität: max. gleichzeitige Worker-Pods (default: 1, max: 100)
  # Default 1 optimiert für Quay: sequentielles Mirroring ermöglicht
  # Blob-Mount (Zero-Copy) von zuvor gepushten Blobs.
  concurrency: 1

  # Images pro Worker-Pod (default: 50, max: 100)
  batchSize: 50

  # Resource-Server-Exposure (optional)
  # Auf OpenShift wird automatisch eine Route erstellt, wenn nicht konfiguriert.
  expose:
    # Optionen: Route (default auf OpenShift), Ingress, GatewayAPI, Service
    type: Route
    # Externer Hostname (optional — bei Route wird automatisch generiert)
    # host: "mirror-resources.example.com"

  # Ressourcen für Worker-Pods (optional)
  worker:
    resources:
      requests: { cpu: "200m", memory: "256Mi" }
      limits:   { cpu: "1000m", memory: "1Gi" }
    nodeSelector: {}
    tolerations: []
```

### ImageSet

Definiert welche Inhalte gespiegelt werden sollen. Ein ImageSet wird über das
`imageSets`-Feld des MirrorTargets zugeordnet — es enthält selbst keinen
Verweis auf ein Ziel.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-21-sync
  namespace: oc-mirror-system
spec:
  mirror:
    # OpenShift / OKD Platform Releases
    platform:
      architectures: ["amd64"]
      kubeVirtContainer: true  # KubeVirt Container-Disk-Images mit spiegeln
      channels:
        - name: stable-4.21
          type: ocp
          minVersion: "4.21.0"
          maxVersion: "4.21.9"
          shortestPath: true

    # OLM Operator-Kataloge (mit automatischer Dependency-Resolution)
    operators:
      - catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21"
        packages:
          - name: odf-operator
            # Dependencies (odf-dependencies, cephcsi-operator, etc.)
            # werden automatisch aufgelöst und mit gespiegelt

    # Einzelne zusätzliche Images
    additionalImages:
      - name: "registry.redhat.io/ubi9/ubi:latest"
      - name: "quay.io/prometheus/prometheus:v2.45.0"
        targetRepo: "custom/prometheus"
        targetTag: "v2.45.0"
```

**Status-Felder:**

| Feld | Beschreibung |
|------|-------------|
| `status.totalImages` | Gesamtanzahl zu spiegelnder Images |
| `status.mirroredImages` | Erfolgreich gespiegelte Images |
| `status.pendingImages` | Ausstehende Images |
| `status.failedImages` | Fehlgeschlagene Images |
| `status.conditions[Ready]` | `True` wenn Collection erfolgreich |

**Image-States (im ConfigMap-State):**

| State | Bedeutung |
|-------|-----------|
| `Pending` | Warte auf Worker-Pod |
| `Mirrored` | Erfolgreich gespiegelt (Digest verifiziert) |
| `Failed` | Fehler — wird bis zu 10× automatisch wiederholt |

---

## Zielpfad-Mapping

Der Operator bildet Quell-Images wie folgt auf Ziel-Pfade ab:

| Typ | Quelle | Ziel |
|-----|--------|------|
| **Release-Payload** | `quay.io/openshift-release-dev/ocp-release:4.21.9-x86_64` | `registry/openshift-release-dev/ocp-release:4.21.9` |
| **Release-Component** | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123...` | `registry/openshift-release-dev/ocp-v4.0-art-dev:sha256-abc123...` |
| **KubeVirt Disk** | `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:5ce03d...` | `registry/openshift-release-dev/ocp-v4.0-art-dev:sha256-5ce03d...` |
| **Operator-Bundle** | `registry.redhat.io/openshift-gitops-1/argocd-rhel8@sha256:def456...` | `registry/openshift-gitops-1/argocd-rhel8:sha256-def456...` |
| **Additional Image** | `quay.io/prometheus/prometheus:v2.45.0` | `registry/prometheus/prometheus:v2.45.0` |

Dabei wird `registry` durch den Wert aus `MirrorTarget.spec.registry` ersetzt. Der Upstream-Repository-Pfad bleibt erhalten.

---

## Blob-Buffering für große Images

Worker-Pods verwenden ein **emptyDir Ephemeral Volume** (`/tmp/blob-buffer`) um große Blobs (>100 MiB) vor dem Upload zwischenzuspeichern. Damit wird ein Quay-spezifisches Problem umgangen, bei dem Upload-Sessions während langsamer Cross-Registry-Transfers ablaufen.

Der Ablauf für große Blobs:
1. Blob wird von der Quell-Registry heruntergeladen und in eine Temp-Datei geschrieben
2. Monolithischer PUT von der lokalen Datei zur Ziel-Registry (schnell)
3. Temp-Datei wird nach dem Upload gelöscht

Vorteile gegenüber RAM-Buffering: Kein OOM-Risiko bei Multi-GB Layern.

---

## Drift Detection

Der Manager überprüft alle **5 Minuten**, ob alle als `Mirrored` markierten Images noch in der Ziel-Registry existieren (HEAD-Request auf das Manifest). Fehlende Images werden automatisch wieder in die Queue gestellt und neu gespiegelt.

### Auth-Token-Refresh

Registry-Clients (regclient) akkumulieren OAuth-Scopes pro Repository in einem einzigen Bearer-Token. Nach ~40 Repositories überschreitet der Token das **nginx-Header-Limit** (8 KB) von Quay-Proxies, was zu `HTTP 400`-Fehlern führt.

**Lösung**: Der Manager erstellt alle **20 CheckExist-Aufrufe** einen frischen Registry-Client mit leerem Token-Cache. Zusätzlich wird zu Beginn jedes Drift-Cycles der Mirror-Client erneuert.

### Fehlerbehandlung

| Szenario | Verhalten |
|----------|----------|
| Image nicht gefunden (404) | Als `Pending` markiert → wird neu gespiegelt |
| Auth-/Netzwerk-Fehler | Image als **vorhanden** angenommen (konservativ) |
| Registry unreachbar | Drift-Check wird übersprungen, nächster Cycle in 5 Min. |

---

## Voraussetzungen

- Kubernetes ≥ 1.26 oder OpenShift ≥ 4.13
- Zugriff auf Quell-Registries (ggf. Pull-Secret im Cluster)
- Schreibzugriff auf die Ziel-Registry (via `authSecret`)

---

## Installation

### 1. CRDs und Operator deployen

```bash
make install
export IMG=my-registry.example.com/oc-mirror-operator:v0.0.1
make deploy IMG=$IMG
```

### 2. Registry-Credentials erstellen

```bash
kubectl create secret docker-registry target-registry-creds \
  --docker-server=registry.example.com \
  --docker-username=<user> \
  --docker-password=<password> \
  -n oc-mirror-system
```

### 3. MirrorTarget und ImageSet anwenden

```bash
kubectl apply -f config/samples/mirror_target_sample.yaml
kubectl apply -f config/samples/imageset_full_sample.yaml
```

### 4. Status überwachen

```bash
# Überblick
kubectl get mirrortarget,imageset -n oc-mirror-system

# Fortschritt
kubectl describe imageset ocp-4-21-sync -n oc-mirror-system

# Manager-Logs
kubectl logs deployment/internal-registry-manager -n oc-mirror-system -f

# Worker-Pods beobachten
kubectl get pods -l app=oc-mirror-worker -n oc-mirror-system -w
```

---

## Sicherheitsmodell

### RBAC
Der Operator läuft mit einer **namespace-scoped `Role`** (nicht `ClusterRole`). Jede Schicht hat einen dedizierten Service Account mit minimalen Rechten:

| Service Account | Berechtigungen |
|-----------------|----------------|
| `oc-mirror-operator-controller-manager` | CRD-Verwaltung, Deployments, Services, ConfigMaps, Secrets (read), Routes, Ingresses |
| `oc-mirror-coordinator` | ImageSet-Status schreiben, Pods erstellen/löschen, ConfigMaps lesen/schreiben, MirrorTargets lesen |
| `oc-mirror-worker` | Keine Cluster-Rechte |

### Pod Security
Alle dynamisch erstellten Pods (Manager und Worker) laufen mit **restricted Pod Security Standards**:
- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `capabilities: drop: ["ALL"]`
- `seccompProfile: RuntimeDefault`

### Worker-Authentifizierung
Worker-Pods authentifizieren sich am Manager-Status-Endpoint via **Bearer Token**. Der Token wird beim Manager-Start zufällig generiert und über eine Umgebungsvariable an Worker-Pods übergeben.

### Registry-Credentials
Das `authSecret` wird als Volume (`/docker-config/config.json`) in Manager- und Worker-Pods gemountet. Der Manager benötigt Lesezugriff für Drift-Detection (CheckExist), Worker benötigen Lese-/Schreibzugriff für das eigentliche Mirroring.

---

## Entwicklung

### Voraussetzungen

```bash
go version     # >= 1.25
make --version
kubectl / oc
```

### Lokaler Build

```bash
make test      # Tests + Build
make build     # Nur bauen
make lint      # Linter
```

### Container-Image

```bash
make podman-buildx IMG=my-registry.io/oc-mirror-operator:latest
# oder
make docker-buildx IMG=my-registry.io/oc-mirror-operator:latest
```

### Tests

```bash
# Unit-Tests
go test ./pkg/... -v

# Controller-Tests (envtest)
make setup-envtest   # einmalig
make test
```

### CRD-Manifeste regenerieren

```bash
make manifests   # CRD YAMLs
make generate    # DeepCopy-Methoden
```

---

## Bekannte Einschränkungen

| Einschränkung | Details |
|---------------|---------|
| **Polling-basierter Manager** | 30s-Ticker statt event-driven Reconciliation |
| **In-Memory Worker-Queue** | `inProgress`-Map nicht persistent; bei Manager-Restart werden laufende Worker per Pod-Sync wiederhergestellt |
| **Blocked Images nicht implementiert** | `spec.mirror.blockedImages` wird akzeptiert aber ignoriert |
| **Helm Charts nicht implementiert** | `spec.mirror.helm` API-Typen definiert, aber Collector wertet sie nicht aus |
| **GatewayAPI nicht implementiert** | `spec.expose.type: GatewayAPI` API-Feld definiert, HTTPRoute-Erstellung noch ausstehend |
| **Kein Pruning** | Veraltete Images werden nicht automatisch aus der Ziel-Registry gelöscht |
| **Kein Mirror-to-Disk** | Air-Gap-Transfer über Datenträger ist nicht möglich — der Operator benötigt Netzwerkzugang zu beiden Registries |
| **Kein HA-Modus** | Leader Election konfigurierbar (`--leader-elect`), aber standardmäßig deaktiviert |
| **Kein Catalog-Cache Pre-Build** | Gefilterter Katalog invalidiert den Source-Cache via opaque whiteout und nutzt `--cache-enforce-integrity=false` — Cache wird beim ersten `opm serve` neu gebaut (einige Sekunden Startup-Delay) |

---

## Projektstruktur

```
oc-mirror-operator/
├── api/v1alpha1/              # CRD-Typen (MirrorTarget, ImageSet)
├── cmd/
│   ├── main.go                # Einsprungpunkt: controller | manager | worker
│   └── catalog-builder/       # Catalog-Builder Binary (läuft in K8s Jobs)
├── config/
│   ├── crd/                   # Generierte CRD-Manifeste
│   ├── rbac/                  # Role, RoleBinding, ServiceAccounts
│   ├── manager/               # Operator-Deployment
│   └── samples/               # Beispiel-CRs
├── internal/controller/       # Kubebuilder-Reconciler
│   ├── mirrortarget_controller.go
│   ├── imageset_controller.go
│   └── conditions.go
└── pkg/mirror/
    ├── client/                # MirrorClient (regclient-Wrapper, Blob-Buffering, BlobCopy)
    ├── collector.go           # Image-Liste aus ImageSet-Spec aufbauen
    ├── planner.go             # Blob-Replikationsplanung (Greedy Set-Cover)
    ├── manager/               # Manager-Logik (Worker-Orchestrierung, State, Pod-Cleanup)
    ├── resources/             # Resource Server (HTTP-API) und IDMS/ITMS/Catalog-Generierung
    │   ├── resources.go       # IDMS, ITMS, CatalogSource, ClusterCatalog, Signatur-Generation
    │   └── server.go          # HTTP-Server (:8081), per-ImageSet Endpoints, Ready-Gating
    ├── release/               # Cincinnati-API-Client (Graph, BFS, Signatures)
    ├── catalog/
    │   ├── resolver.go        # FBC-Resolver: FilterFBC, Dependency-Resolution, Image-Build
    │   └── builder/           # Catalog-Builder Job-Management, Build-Signatur
    └── imagestate/            # ConfigMap-basierte State-Persistenz (gzip-JSON)
```
