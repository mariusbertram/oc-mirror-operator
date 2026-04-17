package catalog

import (
	"context"
	"fmt"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/regclient/regclient/types/ref"
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

	// Validate the image reference is parseable before proceeding
	if _, err := ref.New(catalogImage); err != nil {
		return nil, fmt.Errorf("failed to parse catalog image reference: %w", err)
	}

	// Note: In a production implementation, we would pull the image layers,
	// find the /configs directory, and use declcfg.LoadFS.
	// For this implementation, we simulate the extraction of relevant images
	// from the declarative configuration.

	// We'll return the catalog image itself and simulate finding related images.
	// This ensures the catalog image is always mirrored.
	images := []string{catalogImage}

	// Placeholder for the actual extraction logic:
	// 1. Pull layers
	// 2. Load FBC
	// 3. Filter FBC
	// 4. Collect images from Bundles

	// Implementation note: related images are found in cfg.Bundles[i].Images
	// and cfg.Bundles[i].RelatedImages

	return images, nil
}

// FilterFBC implements the in-memory filtering of a declarative configuration
func (r *CatalogResolver) FilterFBC(ctx context.Context, cfg *declcfg.DeclarativeConfig, packages []string) (*declcfg.DeclarativeConfig, error) {
	if len(packages) == 0 {
		return cfg, nil
	}

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

// ExtractImages returns all image references found in the FBC
func (r *CatalogResolver) ExtractImages(cfg *declcfg.DeclarativeConfig) []string {
	imageMap := make(map[string]bool)

	for _, b := range cfg.Bundles {
		if b.Image != "" {
			imageMap[b.Image] = true
		}
		for _, ri := range b.RelatedImages {
			if ri.Image != "" {
				imageMap[ri.Image] = true
			}
		}
	}

	var images []string
	for img := range imageMap {
		images = append(images, img)
	}
	return images
}
