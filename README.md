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
| **Operator-Katalog Mirroring** | ✅ | ✅ | FBC-Parsing, Package-Filterung, Bundle-Image-Extraktion |
| **Gefiltertes Katalog-Image** | ✅ | ✅ | Neues OCI-Image mit gefiltertem FBC-Layer wird gebaut und gepusht |
| **Package/Channel-Filterung** | ✅ | ✅ | Einzelne Packages und Channels selektierbar |
| **Version-Ranges (Operators)** | ✅ | ✅ | `minVersion` / `maxVersion` pro Package oder Channel |
| **Additional Images** | ✅ | ✅ | Einzelne Images mit optionalem `targetRepo` / `targetTag` |
| **Cosign-Signaturen** | ✅ | ✅ | Tag-basierte `.sig` Signaturen werden automatisch mit kopiert |
| **OCI Referrers** | ✅ | ✅ | Attestations, SBOMs über `regclient.ImageWithReferrers()` |
| **Release-Signaturen** | ✅ | ✅ | Download von mirror.openshift.com/pub/openshift-v4/signatures |
| **Inkrementelles Mirroring** | ✅ | ✅ | Bereits gespiegelte Images werden übersprungen (ConfigMap-State) |
| **Registry-Verifikation** | ✗ | ✅ | Manager prüft regelmäßig ob Images in der Ziel-Registry existieren und queued fehlende neu |
| **Automatische Retries** | ✗ | ✅ | Bis zu 10 Wiederholungen pro Image bei Fehlern |
| **Kontinuierliches Mirroring** | ✗ | ✅ | Reconcile-Loop alle 30s — neue Images werden automatisch erkannt und gespiegelt |
| **Deklarativ via CRDs** | ✗ | ✅ | `MirrorTarget` + `ImageSet` Custom Resources |
| **Skalierbare Worker-Pods** | ✗ | ✅ | Konfigurierbare Concurrency (bis 100 Pods) und BatchSize (bis 100 Images/Pod) |
| **Ephemeral-Volume Blob-Buffering** | ✗ | ✅ | Große Blobs (>100 MiB) werden auf emptyDir gepuffert — kein OOM bei Multi-GB Layern |

### ⚠️ Teilweise implementiert

| Feature | oc-mirror CLI | oc-mirror-operator | Status |
|---------|:---:|:---:|--------|
| **IDMS/ITMS-Generierung** | ✅ | ⚠️ | Code vorhanden (`GenerateIDMS()` / `GenerateITMS()`), aber nicht automatisch angewendet oder exportiert |
| **Operator SkipDependencies** | ✅ | ⚠️ | API-Feld definiert, wird im Collector aber nicht ausgewertet |
| **Operator TargetCatalog** | ✅ | ⚠️ | API-Feld definiert, wird bei der Ziel-Berechnung aber nicht verwendet |

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
| **KubeVirt Container** | ✅ | ❌ | `platform.kubeVirtContainer` Feld existiert, wird nicht ausgewertet |

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

---

## Architektur

Die Architektur folgt einem skalierbaren **Drei-Schichten-Modell**:

```
┌─────────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                                 │
│                                                                     │
│  ┌──────────────────┐   watch   ┌──────────────────────────────┐    │
│  │  ImageSet CR     │◄──────────│                              │    │
│  │  MirrorTarget CR │           │   Operator (Control Plane)   │    │
│  └──────────────────┘  reconcile│   internal/controller/       │    │
│                         ───────►│                              │    │
│                                 └──────────────┬───────────────┘    │
│                                                │ creates Deployment │
│                                                ▼                    │
│                                 ┌──────────────────────────────┐    │
│                                 │  Manager Pod (Orchestrator)  │    │
│                                 │  pkg/mirror/manager/         │    │
│                                 │                              │    │
│                                 │  • Lädt Image-State          │    │
│                                 │  • Verwaltet Worker-Queue    │    │
│                                 │  • HTTP Status-API (:8080)   │    │
│                                 │  • Registry-Verifikation     │    │
│                                 └──────┬──────────────┬────────┘    │
│                                        │ creates Pods │ receives    │
│                                        ▼              │ POST /status│
│                                 ┌──────────────┐      │             │
│                                 │ Worker Pod 1 │──────┘             │
│                                 │ Worker Pod 2 │                    │
│                                 │ Worker Pod N │                    │
│                                 └──────┬───────┘                    │
│                                        │ regclient + emptyDir       │
│                                        ▼                            │
│                                 ┌──────────────────────────────┐    │
│                                 │   Ziel-Registry              │    │
│                                 └──────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

### Komponenten

| Schicht | Komponente | Beschreibung |
|---------|------------|-------------|
| **Control Plane** | `Operator` (`internal/controller/`) | Überwacht CRs, berechnet Image-Soll-Listen via Cincinnati-API und FBC-Parsing, setzt Status-Conditions |
| **Orchestration** | `Manager` (`pkg/mirror/manager/`) | Ein Deployment pro `MirrorTarget`. Lädt Image-State, startet Worker-Pods, empfängt Ergebnisse via authentifizierter HTTP-API, verifiziert Registry-Zustand |
| **Execution** | `Worker` (kurzlebige Pods) | Kopiert Image-Batches mit `regclient`, puffert große Blobs auf emptyDir, kopiert Signaturen, meldet Status via `POST /status` |
| **State** | ConfigMap (gzip-JSON) | Per-Image Mirroring-Status in Kubernetes ConfigMaps — kein PV/PVC nötig, ~30 Bytes pro Image |

### Datenfluss

1. Nutzer erstellt `MirrorTarget` + `ImageSet` CRs
2. **Operator** löst via Cincinnati-API (Releases) und Catalog-Image (Operators) die vollständige Image-Liste auf und speichert sie als gzip-komprimierte ConfigMap
3. **Manager** lädt den Image-State, prüft ob gespiegelte Images noch in der Ziel-Registry vorhanden sind und startet Worker-Pods für ausstehende Images
4. **Worker** kopiert Images (inkl. Signaturen und Referrers), puffert große Blobs auf Ephemeral Volume und meldet Ergebnisse via `POST /status` an den Manager
5. Manager aktualisiert Image-State und `ImageSet.Status`

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

  # Referenz auf ein Secret mit Registry-Credentials (empfohlen)
  authSecret: "target-registry-creds"

  # Für Registries mit self-signed Zertifikaten
  insecure: false

  # Parallelität: max. gleichzeitige Worker-Pods (default: 20, max: 100)
  concurrency: 20

  # Images pro Worker-Pod (default: 10, max: 100)
  batchSize: 10

  # Ressourcen für Worker-Pods (optional)
  worker:
    resources:
      requests: { cpu: "200m", memory: "256Mi" }
      limits:   { cpu: "1000m", memory: "1Gi" }
    nodeSelector: {}
    tolerations: []
```

### ImageSet

Definiert welche Inhalte gespiegelt werden sollen.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-21-sync
  namespace: oc-mirror-system
spec:
  targetRef: "internal-registry"

  mirror:
    # OpenShift / OKD Platform Releases
    platform:
      architectures: ["amd64"]
      channels:
        - name: stable-4.21
          type: ocp
          minVersion: "4.21.0"
          maxVersion: "4.21.9"
          shortestPath: true

    # OLM Operator-Kataloge
    operators:
      - catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.15"
        packages:
          - name: openshift-gitops-operator
            channels:
              - name: stable

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
| `oc-mirror-operator-controller-manager` | CRD-Verwaltung, Deployments, Services, ConfigMaps, Secrets (read) |
| `oc-mirror-coordinator` | ImageSet-Status schreiben, Pods erstellen/löschen, ConfigMaps lesen/schreiben |
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
Das `authSecret` wird als Volume (`/run/secrets/dockerconfig/config.json`) in Worker-Pods gemountet. Der Manager-Pod hat keinen direkten Registry-Zugriff.

---

## Entwicklung

### Voraussetzungen

```bash
go version     # >= 1.23
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
| **IDMS/ITMS nicht angewendet** | Generierungs-Code vorhanden, aber nicht in den Reconcile-Loop integriert |
| **Kein Pruning** | Veraltete Images werden nicht automatisch aus der Ziel-Registry gelöscht |
| **Kein Mirror-to-Disk** | Air-Gap-Transfer über Datenträger ist nicht möglich — der Operator benötigt Netzwerkzugang zu beiden Registries |
| **Kein HA-Modus** | Leader Election konfigurierbar (`--leader-elect`), aber standardmäßig deaktiviert |

---

## Projektstruktur

```
oc-mirror-operator/
├── api/v1alpha1/              # CRD-Typen (MirrorTarget, ImageSet)
├── cmd/main.go                # Einsprungpunkt: controller | manager | worker
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
    ├── client/                # MirrorClient (regclient-Wrapper, Blob-Buffering)
    ├── collector.go           # Image-Liste aus ImageSet-Spec aufbauen
    ├── manager/               # Manager-Logik (Worker-Orchestrierung, State)
    ├── release/               # Cincinnati-API-Client (Graph, BFS, Signatures)
    ├── catalog/               # FBC-Resolver + Catalog-Builder
    ├── imagestate/            # ConfigMap-basierte State-Persistenz (gzip-JSON)
    └── idms_itms.go           # IDMS/ITMS-Generierung (noch nicht integriert)
```
