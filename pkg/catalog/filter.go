package catalog

import (
	"context"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// FilterCatalog filters an OLM FBC based on the provided packages
func FilterCatalog(ctx context.Context, fullConfig *declcfg.DeclarativeConfig, packages []string) (*declcfg.DeclarativeConfig, error) {
	filtered := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{},
		Channels: []declcfg.Channel{},
		Bundles:  []declcfg.Bundle{},
	}

	pkgMap := make(map[string]bool)
	for _, p := range packages {
		pkgMap[p] = true
	}

	// 1. Filter Packages
	for _, p := range fullConfig.Packages {
		if pkgMap[p.Name] {
			filtered.Packages = append(filtered.Packages, p)
		}
	}

	// 2. Filter Channels
	for _, c := range fullConfig.Channels {
		if pkgMap[c.Package] {
			filtered.Channels = append(filtered.Channels, c)
		}
	}

	// 3. Filter Bundles
	for _, b := range fullConfig.Bundles {
		if pkgMap[b.Package] {
			filtered.Bundles = append(filtered.Bundles, b)
		}
	}

	return filtered, nil
}
