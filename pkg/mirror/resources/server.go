package resources

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// ResourceIndex describes available resources for an ImageSet.
type ResourceIndex struct {
	ImageSets []ImageSetIndex `json:"imageSets"`
}

type ImageSetIndex struct {
	Name      string   `json:"name"`
	Ready     bool     `json:"ready"`
	Resources []string `json:"resources"`
	Catalogs  []string `json:"catalogs,omitempty"`
}

// CatalogInfo derives target catalog image reference and display name.
type CatalogInfo struct {
	SourceCatalog string
	TargetImage   string
	DisplayName   string
}

// ExtractCatalogs derives CatalogInfo from an ImageSet's operator spec and the target registry.
func ExtractCatalogs(is *mirrorv1alpha1.ImageSet, registryPrefix string) []CatalogInfo {
	result := make([]CatalogInfo, 0, len(is.Spec.Mirror.Operators))
	for _, op := range is.Spec.Mirror.Operators {
		if op.Catalog == "" {
			continue
		}
		result = append(result, CatalogInfo{
			SourceCatalog: op.Catalog,
			TargetImage:   CatalogTargetRef(registryPrefix, op),
			DisplayName:   CatalogDisplayName(op.Catalog),
		})
	}
	return result
}

// CatalogTargetRef computes the target catalog image reference.
func CatalogTargetRef(registry string, op mirrorv1alpha1.Operator) string {
	tag := op.TargetTag
	if tag == "" {
		catalogForTag := op.Catalog
		if i := strings.Index(catalogForTag, "@"); i >= 0 {
			catalogForTag = catalogForTag[:i]
		}
		if i := strings.LastIndex(catalogForTag, ":"); i >= 0 && !strings.Contains(catalogForTag[i:], "/") {
			tag = catalogForTag[i+1:]
		}
	}
	if tag == "" {
		tag = "latest"
	}
	if op.TargetCatalog != "" {
		return fmt.Sprintf("%s/%s:%s", registry, op.TargetCatalog, tag)
	}
	parts := strings.SplitN(op.Catalog, "/", 2)
	path := op.Catalog
	if len(parts) == 2 {
		path = parts[1]
	}
	if i := strings.IndexAny(path, ":@"); i >= 0 {
		path = path[:i]
	}
	return fmt.Sprintf("%s/%s:%s", registry, path, tag)
}

func CatalogDisplayName(source string) string {
	repo := RepoOnly(source)
	parts := strings.Split(repo, "/")
	if len(parts) > 0 {
		name := parts[len(parts)-1]
		return fmt.Sprintf("OC Mirror - %s", name)
	}
	return "OC Mirror Catalog"
}

// CatalogSlug creates a URL-safe short name from a catalog reference.
func CatalogSlug(source string) string {
	repo := RepoOnly(source)
	parts := strings.Split(repo, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "unknown"
}

// --- Catalog packages response types ---

type CatalogPackagesResponse struct {
	Catalog     string           `json:"catalog"`
	TargetImage string           `json:"targetImage"`
	Packages    []PackageSummary `json:"packages"`
}

type PackageSummary struct {
	Name           string           `json:"name"`
	DefaultChannel string           `json:"defaultChannel,omitempty"`
	Channels       []ChannelSummary `json:"channels"`
}

type ChannelSummary struct {
	Name    string        `json:"name"`
	Entries []BundleEntry `json:"entries"`
}

type BundleEntry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func BuildCatalogPackagesResponse(cat CatalogInfo, cfg *declcfg.DeclarativeConfig) CatalogPackagesResponse {
	channelsByPkg := make(map[string][]declcfg.Channel)
	for _, ch := range cfg.Channels {
		channelsByPkg[ch.Package] = append(channelsByPkg[ch.Package], ch)
	}

	bundleVersions := make(map[string]string, len(cfg.Bundles))
	for _, b := range cfg.Bundles {
		bundleVersions[b.Name] = BundleVersion(b)
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

func BundleVersion(b declcfg.Bundle) string {
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

func RepoOnly(ref string) string {
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		return ref[:i]
	}
	return ref
}
