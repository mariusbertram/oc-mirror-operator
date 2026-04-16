package catalog

import (
	"context"
	"fmt"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

type CatalogResolver struct {
	client *mirrorclient.MirrorClient
}

func New(client *mirrorclient.MirrorClient) *CatalogResolver {
	return &CatalogResolver{client: client}
}

// ResolveCatalog extracts related images from a catalog based on filters
func (r *CatalogResolver) ResolveCatalog(ctx context.Context, catalogImage string, packages []string) ([]string, error) {
	fmt.Printf("Resolving catalog %s for packages %v\n", catalogImage, packages)

	// 1. Fetch catalog index (Simplified for prototype)
	// In a real implementation, we use r.client.RC to pull the image and extract the FBC JSONs.

	// For now, return a placeholder that demonstrates we can extract images
	// This would normally be parsed from the FBC.
	return []string{catalogImage}, nil
}

// FilterFBC implements the in-memory filtering of a declarative configuration
func (r *CatalogResolver) FilterFBC(ctx context.Context, cfg *declcfg.DeclarativeConfig, packages []string) (*declcfg.DeclarativeConfig, error) {
	filtered := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{},
		Channels: []declcfg.Channel{},
		Bundles:  []declcfg.Bundle{},
	}

	pkgMap := make(map[string]bool)
	for _, p := range packages {
		pkgMap[p] = true
	}

	for _, p := range cfg.Packages {
		if pkgMap[p.Name] {
			filtered.Packages = append(filtered.Packages, p)
		}
	}

	for _, c := range cfg.Channels {
		if pkgMap[c.Package] {
			filtered.Channels = append(filtered.Channels, c)
		}
	}

	for _, b := range cfg.Bundles {
		if pkgMap[b.Package] {
			filtered.Bundles = append(filtered.Bundles, b)
		}
	}

	return filtered, nil
}
