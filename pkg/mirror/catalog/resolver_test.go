package catalog

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
)

func TestFilterFBC_PackageRequired(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "operator-a"},
			{Name: "operator-b"},
			{Name: "operator-c"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "operator-a"},
			{Name: "stable", Package: "operator-b"},
			{Name: "stable", Package: "operator-c"},
		},
		Bundles: []declcfg.Bundle{
			{
				Name:    "operator-a.v1.0.0",
				Package: "operator-a",
				Image:   "registry.example.com/a@sha256:aaa",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"operator-a","version":"1.0.0"}`)},
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"operator-b","versionRange":">=1.0.0"}`)},
				},
			},
			{
				Name:    "operator-b.v1.0.0",
				Package: "operator-b",
				Image:   "registry.example.com/b@sha256:bbb",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"operator-b","version":"1.0.0"}`)},
				},
			},
			{
				Name:    "operator-c.v1.0.0",
				Package: "operator-c",
				Image:   "registry.example.com/c@sha256:ccc",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"operator-c","version":"1.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []string{"operator-a"})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Packages) != 2 {
		t.Errorf("expected 2 packages (a + b dependency), got %d", len(filtered.Packages))
	}
	pkgNames := map[string]bool{}
	for _, p := range filtered.Packages {
		pkgNames[p.Name] = true
	}
	if !pkgNames["operator-a"] || !pkgNames["operator-b"] {
		t.Errorf("expected operator-a and operator-b, got %v", pkgNames)
	}
	if pkgNames["operator-c"] {
		t.Error("operator-c should not be included (no dependency)")
	}
}

func TestFilterFBC_GVKRequired(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "consumer-op"},
			{Name: "provider-op"},
			{Name: "unrelated-op"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "consumer-op"},
			{Name: "stable", Package: "provider-op"},
			{Name: "stable", Package: "unrelated-op"},
		},
		Bundles: []declcfg.Bundle{
			{
				Name:    "consumer-op.v1.0.0",
				Package: "consumer-op",
				Image:   "registry.example.com/consumer@sha256:111",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"consumer-op","version":"1.0.0"}`)},
					{Type: olmGVKRequired, Value: json.RawMessage(`{"group":"storage.example.com","version":"v1","kind":"StorageCluster"}`)},
				},
			},
			{
				Name:    "provider-op.v1.0.0",
				Package: "provider-op",
				Image:   "registry.example.com/provider@sha256:222",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"provider-op","version":"1.0.0"}`)},
					{Type: olmGVK, Value: json.RawMessage(`{"group":"storage.example.com","version":"v1","kind":"StorageCluster"}`)},
				},
			},
			{
				Name:    "unrelated-op.v1.0.0",
				Package: "unrelated-op",
				Image:   "registry.example.com/unrelated@sha256:333",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"unrelated-op","version":"1.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []string{"consumer-op"})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Packages) != 2 {
		t.Errorf("expected 2 packages (consumer + provider via GVK), got %d", len(filtered.Packages))
	}
	pkgNames := map[string]bool{}
	for _, p := range filtered.Packages {
		pkgNames[p.Name] = true
	}
	if !pkgNames["consumer-op"] || !pkgNames["provider-op"] {
		t.Errorf("expected consumer-op and provider-op, got %v", pkgNames)
	}
	if pkgNames["unrelated-op"] {
		t.Error("unrelated-op should not be included")
	}
}

func TestFilterFBC_TransitiveChain(t *testing.T) {
	// A requires B (via package), B requires C (via GVK)
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "op-a"},
			{Name: "op-b"},
			{Name: "op-c"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "op-a"},
			{Name: "stable", Package: "op-b"},
			{Name: "stable", Package: "op-c"},
		},
		Bundles: []declcfg.Bundle{
			{
				Name:    "op-a.v1.0.0",
				Package: "op-a",
				Image:   "registry.example.com/a@sha256:a1",
				Properties: []property.Property{
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"op-b"}`)},
				},
			},
			{
				Name:    "op-b.v1.0.0",
				Package: "op-b",
				Image:   "registry.example.com/b@sha256:b1",
				Properties: []property.Property{
					{Type: olmGVKRequired, Value: json.RawMessage(`{"group":"example.com","version":"v1","kind":"Widget"}`)},
				},
			},
			{
				Name:    "op-c.v1.0.0",
				Package: "op-c",
				Image:   "registry.example.com/c@sha256:c1",
				Properties: []property.Property{
					{Type: olmGVK, Value: json.RawMessage(`{"group":"example.com","version":"v1","kind":"Widget"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []string{"op-a"})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Packages) != 3 {
		t.Errorf("expected 3 packages (a→b→c transitive), got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_EmptyPackages(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op-a"}},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []string{})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Packages) != 1 {
		t.Errorf("expected all packages when no filter, got %d", len(filtered.Packages))
	}
}

func TestExtractImages(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Bundles: []declcfg.Bundle{
			{
				Name:    "op.v1.0.0",
				Package: "op",
				Image:   "registry.example.com/op-bundle@sha256:aaa",
				RelatedImages: []declcfg.RelatedImage{
					{Name: "op-image", Image: "registry.example.com/op@sha256:bbb"},
					{Name: "sidecar", Image: "registry.example.com/sidecar@sha256:ccc"},
				},
			},
			{
				Name:    "op.v2.0.0",
				Package: "op",
				Image:   "registry.example.com/op-bundle@sha256:ddd",
				RelatedImages: []declcfg.RelatedImage{
					{Name: "op-image", Image: "registry.example.com/op@sha256:bbb"}, // duplicate
					{Name: "sidecar", Image: "registry.example.com/sidecar@sha256:eee"},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	images := resolver.ExtractImages(cfg)

	// Should deduplicate: 2 bundle images + 3 unique related images = 5
	if len(images) != 5 {
		t.Errorf("expected 5 unique images, got %d: %v", len(images), images)
	}
}
