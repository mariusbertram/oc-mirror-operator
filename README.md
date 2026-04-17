# oc-mirror-operator

Der `oc-mirror-operator` ist ein Kubernetes-Operator zur Automatisierung und kontinuierlichen Spiegelung von OpenShift Releases, Operator-Katalogen und zusätzlichen Container-Images in eine private Registry.

Im Gegensatz zum statischen `oc-mirror` CLI-Tool arbeitet dieser Operator cloud-nativ und deklarativ. Er orchestriert Mirroring-Workflows direkt im Cluster — ohne persistenten Storage, ohne externe Abhängigkeiten und mit voller Kubernetes-Integration über Custom Resources.

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
│                                 └──────────────┬─────────────-─┘    │
│                                                │ creates Deployment │
│                                                ▼                    │
│                                 ┌──────────────────────────────┐    │
│                                 │  Manager Pod (Orchestrator)  │    │
│                                 │  pkg/mirror/manager/         │    │
│                                 │                              │    │
│                                 │  • Liest OCI-State           │    │
│                                 │  • Verwaltet Worker-Queue    │    │
│                                 │  • HTTP Status-API (:8080)   │    │
│                                 └──────┬──────────────┬────────┘    │
│                                        │ creates Pods │ receives    │
│                                        ▼              │ POST /status│
│                                 ┌──────────────┐      │             │
│                                 │ Worker Pod 1 │──────┘             │
│                                 │ Worker Pod 2 │                    │
│                                 │ Worker Pod N │                    │
│                                 │ (max 10)     │                    │
│                                 └──────┬───────┘                    │
│                                        │ kopiert Images             │
│                                        ▼                            │
│                                 ┌──────────────────────────────┐    │
│                                 │   Ziel-Registry              │    │
│                                 │   + OCI Metadata Blob        │    │
│                                 └──────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

### Komponenten

| Schicht | Komponente | Beschreibung |
|---------|------------|-------------|
| **Control Plane** | `Operator` (`internal/controller/`) | Überwacht CRs, berechnet Image-Soll-Listen via Cincinnati-API und FBC-Parsing, setzt Status-Conditions |
| **Orchestration** | `Manager` (`pkg/mirror/manager/`) | Ein Deployment pro `MirrorTarget`. Liest OCI-State, startet Worker-Pods, empfängt Ergebnisse via authentifizierter HTTP-API |
| **Execution** | `Worker` (kurzlebige Pods) | Kopiert jeweils ein Image mit `regclient`, meldet Digest und Status via `POST /status` an den Manager |
| **State** | OCI Metadata Blob | Mirroring-Status wird als OCI-Artifact in der Ziel-Registry gespeichert — kein PV/PVC nötig |

### Datenfluss

1. Nutzer erstellt `MirrorTarget` + `ImageSet` CRs
2. **Operator** löst via Cincinnati-API (Releases) und Catalog-Image (Operators) die vollständige Image-Liste auf und schreibt sie als `Status.TargetImages`
3. **Manager** liest die `ImageSet`-Status-Liste, prüft den OCI-State auf bereits gespiegelte Images und startet Worker-Pods für ausstehende Images (max. 10 parallel)
4. **Worker** kopiert das Image, ruft `GET /status` des Managers mit Digest und `Bearer`-Token auf
5. Manager aktualisiert OCI-State und `ImageSet.Status.MirroredImages`

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
  # Ziel-Registry (Pflicht)
  registry: "registry.example.com/mirror"

  # Referenz auf ein Secret mit Registry-Credentials (empfohlen)
  # Das Secret muss ein .dockerconfigjson-Key enthalten
  authSecret: "target-registry-creds"

  # Für Registries mit self-signed Zertifikaten
  insecure: false

  # Ressourcen für den Manager-Pod (optional)
  manager:
    resources:
      requests: { cpu: "100m", memory: "128Mi" }
      limits:   { cpu: "500m", memory: "512Mi" }
    nodeSelector:
      kubernetes.io/arch: amd64

  # Ressourcen für die Worker-Pods (optional)
  worker:
    resources:
      requests: { cpu: "200m", memory: "256Mi" }
      limits:   { cpu: "1000m", memory: "1Gi" }
```

**Status-Felder:**

| Feld | Beschreibung |
|------|-------------|
| `status.conditions[Ready]` | `True` wenn Manager-Deployment aktiv, `False` bei Fehler |

### ImageSet

Definiert welche Inhalte gespiegelt werden sollen.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-4-15-sync
  namespace: oc-mirror-system
spec:
  # Referenz auf einen MirrorTarget im selben Namespace (Pflicht)
  targetRef: "internal-registry"

  mirror:
    # OpenShift / OKD Platform Releases
    platform:
      architectures: ["amd64", "arm64"]
      channels:
        - name: stable-4.15
          type: ocp                # ocp (default) oder okd
          minVersion: "4.15.0"    # optional: Untergrenze
          maxVersion: "4.15.12"   # Obergrenze
          shortestPath: true       # Nur Upgrade-Pfad-Nodes (BFS über Cincinnati-Graph)
          # full: true             # Alternativ: alle Nodes im Channel

    # OLM Operator-Kataloge
    operators:
      - catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.15"
        packages:
          - name: openshift-gitops-operator
            channels:
              - name: stable
          - name: compliance-operator
            minVersion: "5.0.0"
            maxVersion: "6.0.0"

    # Einzelne zusätzliche Images
    additionalImages:
      - name: "registry.redhat.io/ubi9/ubi:latest"
      - name: "quay.io/prometheus/prometheus@sha256:abc123..."
        targetRepo: "mirror/prometheus"
        targetTag: "v2.45.0"

    # Images, die von der Spiegelung ausgeschlossen werden sollen
    blockedImages:
      - name: "registry.redhat.io/example/unwanted:latest"
```

**Status-Felder:**

| Feld | Beschreibung |
|------|-------------|
| `status.totalImages` | Gesamtanzahl zu spiegelnder Images |
| `status.mirroredImages` | Erfolgreich gespiegelte Images |
| `status.observedGeneration` | Zuletzt reconciliierte Spec-Generation |
| `status.targetImages[]` | Details pro Image: `source`, `destination`, `state` (`Pending`/`Mirrored`/`Failed`), `lastError` |
| `status.conditions[Ready]` | `True` wenn Collection erfolgreich, `False` mit `reason` bei Fehler |
| `status.stateDigest` | Digest des OCI-Metadaten-Blobs in der Ziel-Registry |

**Image-States:**

| State | Bedeutung |
|-------|-----------|
| `Pending` | Warte auf Worker-Pod |
| `Mirrored` | Erfolgreich gespiegelt (Digest bekannt) |
| `Failed` | Fehler — wird bis zu 3× automatisch wiederholt (`lastError` enthält Details) |

---

## Voraussetzungen

- Kubernetes ≥ 1.26 oder OpenShift ≥ 4.13
- `CONTROLLER_IMAGE` Umgebungsvariable **muss** gesetzt sein (kein Default)
- Zugriff auf Quell-Registries (ggf. Pull-Secret im Cluster)
- Schreibzugriff auf die Ziel-Registry (via `authSecret`)

---

## Installation

### 1. CRDs und Operator deployen

```bash
# CRDs installieren
make install

# Operator deployen (setzt CONTROLLER_IMAGE voraus)
export IMG=my-registry.example.com/oc-mirror-operator:v0.0.1
make deploy IMG=$IMG
```

### 2. Registry-Credentials erstellen

```bash
# Docker-Config als Secret (für die Ziel-Registry)
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
kubectl describe imageset ocp-4-15-sync -n oc-mirror-system

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
| `oc-mirror-operator-controller-manager` | CRD-Verwaltung, Deployments, Services, Secrets (read) |
| `oc-mirror-coordinator` | ImageSet-Status schreiben, Pods erstellen/löschen |
| `oc-mirror-worker` | Keine Cluster-Rechte (`automountServiceAccountToken: false`) |

### Pod Security
Alle dynamisch erstellten Pods (Manager und Worker) laufen mit **restricted Pod Security Standards**:
- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `capabilities: drop: ["ALL"]`
- `seccompProfile: RuntimeDefault`

### Worker-Authentifizierung
Worker-Pods authentifizieren sich am Manager-Status-Endpoint via **Bearer Token**. Der Token wird beim Manager-Start zufällig generiert und über eine Umgebungsvariable an Worker-Pods übergeben. Jeder Fake-Status-Request ohne gültigen Token wird mit HTTP 401 abgewiesen.

### Registry-Credentials
Das `authSecret` aus `MirrorTarget.spec.authSecret` wird als Volume (`/run/secrets/dockerconfig/config.json`) in Worker-Pods gemountet. Der Manager-Pod hat keinen direkten Registry-Zugriff.

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
# Alle Tests + Build
make test

# Nur bauen
make build

# Linter
make lint
```

### Multi-Architektur Container-Image

```bash
# Podman (empfohlen für OpenShift-Umgebungen)
make podman-buildx IMG=my-registry.io/oc-mirror-operator:latest

# Docker
make docker-buildx IMG=my-registry.io/oc-mirror-operator:latest
```

### Unit-Tests

```bash
# Alle pkg/ Unit-Tests mit Coverage
go test ./pkg/... -coverprofile cover.out
go tool cover -func cover.out

# Nur Manager-Tests
go test ./pkg/mirror/manager/... -v

# Nur Release-Resolver-Tests
go test ./pkg/mirror/release/... -v
```

### Controller-Tests (envtest)

Die Controller-Tests nutzen `envtest` (embedded etcd + kube-apiserver):

```bash
# envtest-Binaries herunterladen (einmalig)
make setup-envtest

# Tests ausführen
make test
```

### CRD-Manifeste regenerieren

Nach Änderungen an den API-Typen in `api/v1alpha1/`:

```bash
make manifests   # CRD YAMLs regenerieren
make generate    # DeepCopy-Methoden regenerieren
```

---

## Bekannte Einschränkungen

| Einschränkung | Details |
|---------------|---------|
| **Polling-basierter Manager** | Der Manager-Pod verwendet einen 30s-Ticker statt event-driven Reconciliation. Geplante Verbesserung: controller-runtime Controller im Manager. |
| **In-Memory Worker-Queue** | Die `inProgress`-Map im Manager-Pod ist nicht persistent. Bei Pod-Restart werden laufende Worker neu gestartet (idempotent durch `CheckExist`-Prüfung). |
| **FBC-Parsing unvollständig** | `CatalogResolver.ResolveCatalog` gibt aktuell nur das Catalog-Image zurück; vollständige FBC-Layer-Extraktion und Bundle-Image-Auflösung sind in Arbeit. |
| **Kein HA-Modus** | Leader Election ist konfigurierbar (`--leader-elect`), aber standardmäßig deaktiviert. Für Produktivumgebungen aktivieren. |

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
│   └── conditions.go          # Shared Status-Condition Helper
└── pkg/mirror/
    ├── client/                # MirrorClient (regclient wrapper)
    ├── collector.go           # Image-Liste aus ImageSet-Spec aufbauen
    ├── manager/               # Manager-Logik (Worker-Orchestrierung)
    ├── release/               # Cincinnati-API-Client (Version-Range, BFS)
    ├── catalog/               # FBC-Resolver (Operator-Kataloge)
    ├── state/                 # OCI-backed Metadaten-Persistenz
    └── idms_itms.go           # IDMS/ITMS-Generierung für OpenShift
```
