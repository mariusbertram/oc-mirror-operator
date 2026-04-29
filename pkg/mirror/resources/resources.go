// Package resources generates Kubernetes/OpenShift cluster resources
// (IDMS, ITMS, CatalogSource, ClusterCatalog, signature ConfigMaps)
// from the operator's image state and catalog build information.
package resources

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	confv1 "github.com/openshift/api/config/v1"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// --- Image reference parsing helpers ---

// splitImageRef splits an image reference into (registry+repo, tag-or-digest).
// It correctly handles registries with ports (e.g. registry.example.com:5000/repo).
func splitImageRef(ref string) (repo, tagOrDigest string) {
	// Digest-based: split at @
	if idx := strings.Index(ref, "@"); idx != -1 {
		return ref[:idx], ref[idx:]
	}
	// Tag-based: find last colon that is NOT part of a port number.
	// A port is always followed by / in a registry hostname.
	lastColon := strings.LastIndex(ref, ":")
	if lastColon == -1 {
		return ref, ""
	}
	// If there's a / after the colon, the colon is part of a port in the hostname.
	afterColon := ref[lastColon+1:]
	if strings.Contains(afterColon, "/") {
		return ref, ""
	}
	return ref[:lastColon], ":" + afterColon
}

// repoOnly extracts the registry+repository portion from an image reference,
// stripping any tag or digest suffix.
func repoOnly(ref string) string {
	repo, _ := splitImageRef(ref)
	return repo
}

// isDigestRef returns true if the reference contains a digest (@sha256:...).
func isDigestRef(ref string) bool {
	return strings.Contains(ref, "@sha256:")
}

// isTagRef returns true if the reference is tag-based (has :tag, no @digest).
func isTagRef(ref string) bool {
	return !strings.Contains(ref, "@") && strings.Contains(ref, ":")
}

// --- IDMS / ITMS Generation ---

// GenerateIDMS generates an ImageDigestMirrorSet YAML from image state.
// Only includes images that are in "Mirrored" state with digest references.
func GenerateIDMS(name string, state imagestate.ImageState) ([]byte, error) {
	// Group: source-repo → set of mirror-repos
	type mirrorEntry struct {
		source  string
		mirrors map[string]struct{}
	}
	mirrorMap := make(map[string]*mirrorEntry)

	for dest, entry := range state {
		if entry.State != "Mirrored" {
			continue
		}
		if !isDigestRef(entry.Source) {
			continue
		}
		srcRepo := repoOnly(entry.Source)
		destRepo := repoOnly(dest)
		if srcRepo == destRepo {
			continue
		}
		e, ok := mirrorMap[srcRepo]
		if !ok {
			e = &mirrorEntry{source: srcRepo, mirrors: make(map[string]struct{})}
			mirrorMap[srcRepo] = e
		}
		e.mirrors[destRepo] = struct{}{}
	}

	// Sort for deterministic output.
	sources := make([]string, 0, len(mirrorMap))
	for src := range mirrorMap {
		sources = append(sources, src)
	}
	sort.Strings(sources)

	digestMirrors := make([]confv1.ImageDigestMirrors, 0, len(sources))
	for _, src := range sources {
		e := mirrorMap[src]
		mirrors := make([]string, 0, len(e.mirrors))
		for m := range e.mirrors {
			mirrors = append(mirrors, m)
		}
		sort.Strings(mirrors)
		imageMirrors := make([]confv1.ImageMirror, len(mirrors))
		for i, m := range mirrors {
			imageMirrors[i] = confv1.ImageMirror(m)
		}
		digestMirrors = append(digestMirrors, confv1.ImageDigestMirrors{
			Source:  src,
			Mirrors: imageMirrors,
		})
	}

	idms := &confv1.ImageDigestMirrorSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "config.openshift.io/v1",
			Kind:       "ImageDigestMirrorSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: confv1.ImageDigestMirrorSetSpec{
			ImageDigestMirrors: digestMirrors,
		},
	}
	return yaml.Marshal(idms)
}

// GenerateITMS generates an ImageTagMirrorSet YAML from image state.
// Only includes images that are in "Mirrored" state with tag references.
func GenerateITMS(name string, state imagestate.ImageState) ([]byte, error) {
	type mirrorEntry struct {
		source  string
		mirrors map[string]struct{}
	}
	mirrorMap := make(map[string]*mirrorEntry)

	for dest, entry := range state {
		if entry.State != "Mirrored" {
			continue
		}
		if !isTagRef(entry.Source) {
			continue
		}
		srcRepo := repoOnly(entry.Source)
		destRepo := repoOnly(dest)
		if srcRepo == destRepo {
			continue
		}
		e, ok := mirrorMap[srcRepo]
		if !ok {
			e = &mirrorEntry{source: srcRepo, mirrors: make(map[string]struct{})}
			mirrorMap[srcRepo] = e
		}
		e.mirrors[destRepo] = struct{}{}
	}

	sources := make([]string, 0, len(mirrorMap))
	for src := range mirrorMap {
		sources = append(sources, src)
	}
	sort.Strings(sources)

	tagMirrors := make([]confv1.ImageTagMirrors, 0, len(sources))
	for _, src := range sources {
		e := mirrorMap[src]
		mirrors := make([]string, 0, len(e.mirrors))
		for m := range e.mirrors {
			mirrors = append(mirrors, m)
		}
		sort.Strings(mirrors)
		imageMirrors := make([]confv1.ImageMirror, len(mirrors))
		for i, m := range mirrors {
			imageMirrors[i] = confv1.ImageMirror(m)
		}
		tagMirrors = append(tagMirrors, confv1.ImageTagMirrors{
			Source:  src,
			Mirrors: imageMirrors,
		})
	}

	itms := &confv1.ImageTagMirrorSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "config.openshift.io/v1",
			Kind:       "ImageTagMirrorSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: confv1.ImageTagMirrorSetSpec{
			ImageTagMirrors: tagMirrors,
		},
	}
	return yaml.Marshal(itms)
}

// --- CatalogSource / ClusterCatalog Generation ---

// CatalogInfo describes a mirrored operator catalog.
type CatalogInfo struct {
	// SourceCatalog is the original catalog reference (e.g. registry.redhat.io/redhat/redhat-operator-index:v4.21).
	SourceCatalog string
	// TargetImage is the filtered catalog image in the target registry.
	TargetImage string
	// DisplayName is a human-readable name for the catalog.
	DisplayName string
}

// GenerateCatalogSource generates a CatalogSource YAML for OLM v0.
func GenerateCatalogSource(name, namespace string, catalog CatalogInfo, pullSecretName string) ([]byte, error) {
	cs := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"sourceType":  "grpc",
			"image":       catalog.TargetImage,
			"displayName": catalog.DisplayName,
			"publisher":   "oc-mirror-operator",
			"updateStrategy": map[string]interface{}{
				"registryPoll": map[string]interface{}{
					"interval": "10m",
				},
			},
		},
	}
	if pullSecretName != "" {
		cs["spec"].(map[string]interface{})["secrets"] = []string{pullSecretName}
	}
	return yaml.Marshal(cs)
}

// GenerateClusterCatalog generates a ClusterCatalog YAML for OLM v1.
func GenerateClusterCatalog(name string, catalog CatalogInfo) ([]byte, error) {
	cc := map[string]interface{}{
		"apiVersion": "olm.operatorframework.io/v1",
		"kind":       "ClusterCatalog",
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"type": "Image",
				"image": map[string]interface{}{
					"ref": catalog.TargetImage,
				},
			},
		},
	}
	return yaml.Marshal(cc)
}

// --- Release Signature ConfigMaps ---

// SignatureData maps release digests to their signature bytes.
// Key format: "sha256:abc123...", value: raw GPG signature data.
// CatalogTargetImage builds the destination for a catalog image.
func CatalogTargetImage(registry, source string) string {
	return fmt.Sprintf("%s/%s", registry, repoOnly(source))
}

// --- Signature data type ---

type SignatureData map[string][]byte

// GenerateSignatureConfigMaps generates ConfigMap YAMLs in the OpenShift
// release verification format.
// Namespace: openshift-config-managed
// Label: release.openshift.io/verification-signatures=""
func GenerateSignatureConfigMaps(signatures SignatureData) ([]byte, error) {
	if len(signatures) == 0 {
		return []byte("# No release signatures available\n"), nil
	}

	digests := make([]string, 0, len(signatures))
	for d := range signatures {
		digests = append(digests, d)
	}
	sort.Strings(digests)

	docs := make([]string, 0, len(digests))
	for _, digest := range digests {
		sig := signatures[digest]
		// ConfigMap name: sha256-<first 12 hex chars of hash>-1
		hashPart := strings.TrimPrefix(digest, "sha256:")
		cmName := fmt.Sprintf("sha256-%s-1", hashPart[:12])
		// Data key: sha256-<full-hash>-1
		dataKey := fmt.Sprintf("sha256-%s-1", hashPart)

		cm := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: "openshift-config-managed",
				Labels: map[string]string{
					"release.openshift.io/verification-signatures": "",
				},
			},
			BinaryData: map[string][]byte{
				dataKey: sig,
			},
		}

		data, err := yaml.Marshal(cm)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal signature ConfigMap for %s: %w", digest, err)
		}
		docs = append(docs, string(data))
	}

	// Multi-document YAML
	return []byte(strings.Join(docs, "---\n")), nil
}

// GenerateSignatureConfigMapsBase64 is like GenerateSignatureConfigMaps but
// accepts base64-encoded signature data (as stored in ConfigMaps).
func GenerateSignatureConfigMapsBase64(signatures map[string]string) ([]byte, error) {
	raw := make(SignatureData, len(signatures))
	for digest, b64 := range signatures {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode signature for %s: %w", digest, err)
		}
		raw[digest] = data
	}
	return GenerateSignatureConfigMaps(raw)
}

// --- Catalog helpers ---

// CatalogSlug creates a URL-safe short name from a catalog reference.
func CatalogSlug(source string) string {
	repo := repoOnly(source)
	parts := strings.Split(repo, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "unknown"
}

// FindCatalog returns the CatalogInfo matching the given slug.
func FindCatalog(catalogs []CatalogInfo, slug string) (CatalogInfo, bool) {
	for _, c := range catalogs {
		if CatalogSlug(c.SourceCatalog) == slug {
			return c, true
		}
	}
	return CatalogInfo{}, false
}

// --- Catalog packages response types ---

// CatalogPackagesResponse is the JSON envelope for the packages endpoint.
type CatalogPackagesResponse struct {
	Catalog     string           `json:"catalog"`
	TargetImage string           `json:"targetImage"`
	Packages    []PackageSummary `json:"packages"`
}

// PackageSummary describes a single operator package in the catalog.
type PackageSummary struct {
	Name           string           `json:"name"`
	DefaultChannel string           `json:"defaultChannel,omitempty"`
	Channels       []ChannelSummary `json:"channels"`
}

// ChannelSummary describes a single channel within an operator package.
type ChannelSummary struct {
	Name    string        `json:"name"`
	Entries []BundleEntry `json:"entries"`
}

// BundleEntry describes a single bundle version within a channel.
type BundleEntry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// BuildCatalogPackagesResponse builds a structured packages response from catalog info and FBC config.
func BuildCatalogPackagesResponse(cat CatalogInfo, cfg *declcfg.DeclarativeConfig) CatalogPackagesResponse {
	channelsByPkg := make(map[string][]declcfg.Channel)
	for _, ch := range cfg.Channels {
		channelsByPkg[ch.Package] = append(channelsByPkg[ch.Package], ch)
	}

	bundleVersions := make(map[string]string, len(cfg.Bundles))
	for _, b := range cfg.Bundles {
		bundleVersions[b.Name] = bundleVersion(b)
	}

	defaultChannels := make(map[string]string, len(cfg.Packages))
	for _, p := range cfg.Packages {
		defaultChannels[p.Name] = p.DefaultChannel
	}

	pkgNames := make([]string, 0, len(cfg.Packages))
	for _, p := range cfg.Packages {
		pkgNames = append(pkgNames, p.Name)
	}
	sort.Strings(pkgNames)

	packages := make([]PackageSummary, 0, len(pkgNames))
	for _, pkgName := range pkgNames {
		channels := channelsByPkg[pkgName]
		sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })

		chSummaries := make([]ChannelSummary, 0, len(channels))
		for _, ch := range channels {
			entries := make([]BundleEntry, 0, len(ch.Entries))
			for _, e := range ch.Entries {
				entries = append(entries, BundleEntry{
					Name:    e.Name,
					Version: bundleVersions[e.Name],
				})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			chSummaries = append(chSummaries, ChannelSummary{
				Name:    ch.Name,
				Entries: entries,
			})
		}

		packages = append(packages, PackageSummary{
			Name:           pkgName,
			DefaultChannel: defaultChannels[pkgName],
			Channels:       chSummaries,
		})
	}

	return CatalogPackagesResponse{
		Catalog:     cat.SourceCatalog,
		TargetImage: cat.TargetImage,
		Packages:    packages,
	}
}

// bundleVersion extracts the version from a bundle's olm.package property.
func bundleVersion(b declcfg.Bundle) string {
	for _, prop := range b.Properties {
		if prop.Type != "olm.package" {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(prop.Value, &v) == nil && v.Version != "" {
			return v.Version
		}
	}
	return ""
}
