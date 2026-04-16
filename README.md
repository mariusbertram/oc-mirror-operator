# ocp-mirror Operator

`ocp-mirror` ist ein Kubernetes OLM Operator zur automatisierten und kontinuierlichen Spiegelung von OpenShift Releases, Operator Catalogs und zusätzlichen Images in eine private Container Registry.

Im Gegensatz zum statischen `oc-mirror` CLI Tool arbeitet dieser Operator deklarativ, generiert dynamische Spiegelungs-Listen und setzt dabei auf `regclient` für performante und sichere Image-Transfers.

## Features

- **CRD-Basierte Konfiguration:** Nutze die bekannten `ImageSet` Konfigurationen als Kubernetes Custom Resources.
- **Meta-Ziele (MirrorTarget):** Definiere eine `MirrorTarget` CR für die Ziel-Registry und lasse beliebig viele `ImageSet` Ressourcen darauf mappen.
- **Worker-Pool Replikation:** Paralleles Spiegeln von Images inkl. deren Signaturen für maximale Performance.
- **Intelligente Catalog-Filterung:** Spiegelt nur die benötigten Operator-Packages aus einem Catalog und erstellt dynamisch verschlankte OLM Index Images.
- **Digest-Tagging:** Images, die nur mit einem Digest referenziert werden, erhalten diesen Digest automatisch als Tag in der Ziel-Registry.
- **Automatische Cluster-Konfiguration:** Generiert automatisch `ImageDigestMirrorSet` (IDMS) und `ImageTagMirrorSet` (ITMS) für den Cluster.
- **Powered by `regclient`:** Effizienter Transport direkt über die Docker V2 API ohne `skopeo` Dependency.

## Voraussetzungen

- Ein Kubernetes oder OpenShift Cluster
- Operator Lifecycle Manager (OLM) installiert
- Zugriff auf Quell-Registries (z.B. `registry.redhat.io`) und Ziel-Registries

## Installation

```bash
# Installiere die CRDs
make install

# Starte den Operator lokal (für Entwicklung)
make run
```

## Nutzung

1. **MirrorTarget erstellen:**
Definiere die Ziel-Registry und optionale Authentifizierungsdaten.

```bash
kubectl apply -f config/samples/mirror_target_sample.yaml
```

2. **ImageSet definieren:**
Erstelle eine Mirror-Konfiguration, die auf das Ziel verweist.

```bash
kubectl apply -f config/samples/imageset_full_sample.yaml
```

Der Operator berechnet automatisch die Liste der zu spiegelnden Images, führt die Catalog-Filterung durch und repliziert die Images inklusive Signaturen über einen internen Worker-Pool.

### Status überwachen
Den Fortschritt der Spiegelung kannst du direkt in der `ImageSet` Ressource einsehen:

```bash
kubectl get imageset ocp-4-15-sync -o yaml
```

## Entwicklung

Dieses Projekt verwendet das `operator-sdk` (Go).

```bash
# Projekt initialisieren und Abhängigkeiten laden
go mod tidy

# Manifeste generieren
make manifests

# Tests ausführen
make test
```
