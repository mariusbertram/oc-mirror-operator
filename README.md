# oc-mirror-operator

Der `oc-mirror-operator` ist ein Kubernetes-Operator zur Automatisierung und kontinuierlichen Spiegelung von OpenShift Releases, Operator-Katalogen und zusätzlichen Images in eine private Container-Registry.

Im Gegensatz zum statischen `oc-mirror` CLI-Tool arbeitet dieser Operator cloud-nativ und deklarativ. Er wurde speziell entwickelt, um Mirroring-Workflows direkt im Cluster ohne externe Abhängigkeiten oder persistenten Storage zu orchestrieren.

## Architektur

Die Architektur folgt einem skalierbaren **Manager-Worker-Modell**:

1.  **Operator (Control Plane):** Überwacht `ImageSet` und `MirrorTarget` Ressourcen. Er berechnet die "Soll-Liste" der Images durch Integration mit der Cincinnati-API (für OCP Releases) und FBC-Parsing (für Operatoren).
2.  **Manager (Orchestrator):** Pro `MirrorTarget` wird ein Manager-Pod gestartet. Er verwaltet die Queue, prüft den State in der Ziel-Registry und startet Worker-Pods.
3.  **Worker (Data Plane):** Kurzlebige Pods, die die eigentliche Kopie eines einzelnen Images durchführen. Ergebnisse (Digests) werden via Log-Parsing an den Manager zurückgemeldet.
4.  **OCI-backed State:** Der Mirroring-Status ("Was wurde bereits kopiert?") wird als OCI-Blob direkt in der Ziel-Registry gespeichert. Dies macht den Operator vollkommen zustandslos innerhalb des Clusters.

## Kern-Features

- **Cincinnati Integration:** Automatische Auflösung von OpenShift Upgrade-Graphen und Payload-Images.
- **Operator Catalog Filtering:** Präzise Auswahl von Paketen und Kanälen aus OLM-Katalogen (File-Based Catalogs).
- **Zustandsloses Mirroring:** Kein PV/PVC erforderlich; der State "reist" mit den Daten in der Registry.
- **Multi-Arch Support:** Integrierter Support für `amd64`, `arm64`, `ppc64le` und `s390x` via Multi-Arch Builds.
- **Ressourcen-Steuerung:** Requests, Limits und Node-Scheduling für Manager und Worker sind über die `MirrorTarget` CR konfigurierbar.
- **Powered by `regclient`:** Performanter Image-Transfer direkt über die Registry-API.

## Nutzung

### 1. MirrorTarget erstellen
Definiere das Ziel und die Ressourcen-Constraints für die Mirror-Pods.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: MirrorTarget
metadata:
  name: internal-registry
spec:
  registry: "registry.example.com/mirror"
  authSecret: "target-registry-creds"
  manager:
    resources:
      limits: { cpu: "200m", memory: "256Mi" }
  worker:
    resources:
      requests: { cpu: "100m", memory: "128Mi" }
```

### 2. ImageSet definieren
Erstelle die Spiegelungs-Konfiguration.

```yaml
apiVersion: mirror.openshift.io/v1alpha1
kind: ImageSet
metadata:
  name: ocp-release-sync
spec:
  targetRef: "internal-registry"
  mirror:
    platform:
      channels:
        - name: "stable-4.15"
          maxVersion: "4.15.12"
    operators:
      - catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.15"
        packages:
          - name: "compliance-operator"
```

## Entwicklung & Build

### Multi-Architektur Build
Das Projekt nutzt Podman für native Cross-Kompilierung:

```bash
make podman-buildx IMG=my-registry.io/oc-mirror-operator:latest
```

### Tests ausführen
```bash
# Unit-Tests mit Coverage
go test ./pkg/mirror/... -coverprofile cover.out
go tool cover -func cover.out
```
