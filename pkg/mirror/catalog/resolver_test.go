package catalog

import (
	"context"
	"encoding/json"
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
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
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{{Name: "operator-a"}})
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
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{{Name: "consumer-op"}})
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
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{{Name: "op-a"}})
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
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Packages) != 1 {
		t.Errorf("expected all packages when no filter, got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_CompanionDependencyPackage(t *testing.T) {
	// Simulates the Red Hat ODF pattern: odf-operator has no deps,
	// but odf-dependencies package declares olm.package.required for sub-operators.
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "odf-operator"},
			{Name: "odf-dependencies"},
			{Name: "ocs-operator"},
			{Name: "mcg-operator"},
			{Name: "unrelated"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "odf-operator"},
			{Name: "stable", Package: "odf-dependencies"},
			{Name: "stable", Package: "ocs-operator"},
			{Name: "stable", Package: "mcg-operator"},
			{Name: "stable", Package: "unrelated"},
		},
		Bundles: []declcfg.Bundle{
			{
				Name:    "odf-operator.v4.21.0",
				Package: "odf-operator",
				Image:   "registry.example.com/odf-bundle@sha256:111",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"odf-operator","version":"4.21.0"}`)},
					// No olm.package.required — odf-operator handles deps programmatically
				},
			},
			{
				Name:    "odf-dependencies.v4.21.0",
				Package: "odf-dependencies",
				Image:   "registry.example.com/odf-deps-bundle@sha256:222",
				Properties: []property.Property{
					{Type: "olm.package", Value: json.RawMessage(`{"packageName":"odf-dependencies","version":"4.21.0"}`)},
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"ocs-operator","versionRange":">=4.21.0"}`)},
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"mcg-operator","versionRange":">=4.21.0"}`)},
				},
			},
			{
				Name:    "ocs-operator.v4.21.0",
				Package: "ocs-operator",
				Image:   "registry.example.com/ocs-bundle@sha256:333",
			},
			{
				Name:    "mcg-operator.v4.21.0",
				Package: "mcg-operator",
				Image:   "registry.example.com/mcg-bundle@sha256:444",
			},
			{
				Name:    "unrelated.v1.0.0",
				Package: "unrelated",
				Image:   "registry.example.com/unrelated@sha256:555",
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{{Name: "odf-operator"}})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	// Should include: odf-operator + odf-dependencies (companion) + ocs-operator + mcg-operator
	pkgNames := map[string]bool{}
	for _, p := range filtered.Packages {
		pkgNames[p.Name] = true
	}

	if len(filtered.Packages) != 4 {
		t.Errorf("expected 4 packages, got %d: %v", len(filtered.Packages), pkgNames)
	}
	for _, expected := range []string{"odf-operator", "odf-dependencies", "ocs-operator", "mcg-operator"} {
		if !pkgNames[expected] {
			t.Errorf("expected %s to be included, got %v", expected, pkgNames)
		}
	}
	if pkgNames["unrelated"] {
		t.Error("unrelated should not be included")
	}
}

// TestFilterFBC_ChannelFilter verifies that only the requested channel and its
// bundles are included when Channels is specified.
func TestFilterFBC_ChannelFilter(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0"},
				},
			},
			{
				Name:    "preview",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v3.0.0"},
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:100",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)},
				},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:200",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)},
				},
			},
			{
				Name: "my-op.v3.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:300",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"3.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", Channels: []mirrorv1alpha1.IncludeChannel{{Name: "stable"}}},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	if len(filtered.Channels) != 1 {
		t.Errorf("expected 1 channel (stable), got %d", len(filtered.Channels))
	}
	if len(filtered.Channels) > 0 && filtered.Channels[0].Name != "stable" {
		t.Errorf("expected channel 'stable', got %q", filtered.Channels[0].Name)
	}
	// Only stable bundles: v1.0.0 and v2.0.0
	bundleNames := map[string]bool{}
	for _, b := range filtered.Bundles {
		bundleNames[b.Name] = true
	}
	if len(filtered.Bundles) != 2 {
		t.Errorf("expected 2 bundles (v1, v2 from stable), got %d: %v", len(filtered.Bundles), bundleNames)
	}
	if bundleNames["my-op.v3.0.0"] {
		t.Error("preview channel bundle v3.0.0 should not be included")
	}
}

// TestFilterFBC_MinVersionFilter verifies that bundles below minVersion are excluded.
func TestFilterFBC_MinVersionFilter(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0"},
					{Name: "my-op.v3.0.0"},
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:100",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)},
				},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:200",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)},
				},
			},
			{
				Name: "my-op.v3.0.0", Package: "my-op",
				Image: "reg/my-op-bundle@sha256:300",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"3.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "2.0.0"}},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	bundleNames := map[string]bool{}
	for _, b := range filtered.Bundles {
		bundleNames[b.Name] = true
	}
	if bundleNames["my-op.v1.0.0"] {
		t.Error("v1.0.0 is below minVersion 2.0.0 and should be excluded")
	}
	if !bundleNames["my-op.v2.0.0"] {
		t.Error("v2.0.0 should be included (>= minVersion 2.0.0)")
	}
	if !bundleNames["my-op.v3.0.0"] {
		t.Error("v3.0.0 should be included (>= minVersion 2.0.0)")
	}
}

// TestFilterFBC_VersionRange verifies that only bundles within [min,max] are included.
func TestFilterFBC_VersionRange(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0"},
					{Name: "my-op.v3.0.0"},
					{Name: "my-op.v4.0.0"},
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{Name: "my-op.v1.0.0", Package: "my-op", Image: "reg/b@sha256:100",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)}}},
			{Name: "my-op.v2.0.0", Package: "my-op", Image: "reg/b@sha256:200",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)}}},
			{Name: "my-op.v3.0.0", Package: "my-op", Image: "reg/b@sha256:300",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"3.0.0"}`)}}},
			{Name: "my-op.v4.0.0", Package: "my-op", Image: "reg/b@sha256:400",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"4.0.0"}`)}}},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "2.0.0", MaxVersion: "3.0.0"}},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	bundleNames := map[string]bool{}
	for _, b := range filtered.Bundles {
		bundleNames[b.Name] = true
	}
	if bundleNames["my-op.v1.0.0"] {
		t.Error("v1.0.0 should be excluded (< 2.0.0)")
	}
	if !bundleNames["my-op.v2.0.0"] {
		t.Error("v2.0.0 should be included")
	}
	if !bundleNames["my-op.v3.0.0"] {
		t.Error("v3.0.0 should be included")
	}
	if bundleNames["my-op.v4.0.0"] {
		t.Error("v4.0.0 should be excluded (> 3.0.0)")
	}
}

// TestFilterFBC_TransitivePkgAllBundles verifies that transitive deps include
// all bundles regardless of the explicit package's version filter.
func TestFilterFBC_TransitivePkgAllBundles(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "main-op"},
			{Name: "dep-op"},
		},
		Channels: []declcfg.Channel{
			{
				Name: "stable", Package: "main-op",
				Entries: []declcfg.ChannelEntry{{Name: "main-op.v2.0.0"}},
			},
			{
				Name: "stable", Package: "dep-op",
				Entries: []declcfg.ChannelEntry{{Name: "dep-op.v1.0.0"}, {Name: "dep-op.v2.0.0"}},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "main-op.v2.0.0", Package: "main-op", Image: "reg/main@sha256:200",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"main-op","version":"2.0.0"}`)},
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"dep-op"}`)},
				},
			},
			{Name: "dep-op.v1.0.0", Package: "dep-op", Image: "reg/dep@sha256:100",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"dep-op","version":"1.0.0"}`)}}},
			{Name: "dep-op.v2.0.0", Package: "dep-op", Image: "reg/dep@sha256:200",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"dep-op","version":"2.0.0"}`)}}},
		},
	}

	resolver := &CatalogResolver{}
	// Only ask for main-op >= 2.0.0, but dep-op should still include all bundles.
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "main-op", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "2.0.0"}},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	bundleNames := map[string]bool{}
	for _, b := range filtered.Bundles {
		bundleNames[b.Name] = true
	}
	if !bundleNames["dep-op.v1.0.0"] {
		t.Error("dep-op.v1.0.0 should be included as transitive dep (no version filter)")
	}
	if !bundleNames["dep-op.v2.0.0"] {
		t.Error("dep-op.v2.0.0 should be included as transitive dep (no version filter)")
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

// TestFilterFBC_HeadsOnly verifies that when no channels are specified,
// only the channel head (latest version) of every channel is included.
func TestFilterFBC_HeadsOnly(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "my-op", DefaultChannel: "stable"},
		},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0", Replaces: "my-op.v1.0.0"},
				},
			},
			{
				Name:    "preview",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v3.0.0"},
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image: "reg/my-op@sha256:100",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)},
				},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image: "reg/my-op@sha256:200",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)},
				},
			},
			{
				Name: "my-op.v3.0.0", Package: "my-op",
				Image: "reg/my-op@sha256:300",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"3.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	// No channels specified — heads-only: v2.0.0 (stable head) + v3.0.0 (preview head).
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	// Both channels should be included.
	if len(filtered.Channels) != 2 {
		t.Errorf("expected 2 channels (heads-only includes all), got %d", len(filtered.Channels))
	}
	// Channel entries should be trimmed to heads only.
	for _, ch := range filtered.Channels {
		if len(ch.Entries) != 1 {
			t.Errorf("channel %s: expected 1 entry (head only), got %d", ch.Name, len(ch.Entries))
		}
	}

	bundleNames := map[string]bool{}
	for _, b := range filtered.Bundles {
		bundleNames[b.Name] = true
	}
	// Only heads: v2.0.0 + v3.0.0.
	if len(filtered.Bundles) != 2 {
		t.Errorf("expected 2 bundles (heads only), got %d: %v", len(filtered.Bundles), bundleNames)
	}
	if bundleNames["my-op.v1.0.0"] {
		t.Error("v1.0.0 should not be included — it is superseded by v2.0.0")
	}
	if !bundleNames["my-op.v2.0.0"] {
		t.Error("v2.0.0 (stable head) should be included")
	}
	if !bundleNames["my-op.v3.0.0"] {
		t.Error("v3.0.0 (preview head) should be included")
	}
}

// TestFilterFBC_DefaultChannelNoDefaultSet verifies that when no channels are
// specified and the package has no default channel, all channels are included
// as a safe fallback.
func TestFilterFBC_DefaultChannelNoDefaultSet(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "my-op"}, // no DefaultChannel
		},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v1.0.0"}},
			},
			{
				Name:    "preview",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v2.0.0"}},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:100",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)}},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:200",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)}},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	// No default channel → fallback to all channels.
	if len(filtered.Channels) != 2 {
		t.Errorf("expected 2 channels (no default → all), got %d", len(filtered.Channels))
	}
	if len(filtered.Bundles) != 2 {
		t.Errorf("expected 2 bundles (all channels), got %d", len(filtered.Bundles))
	}
}

// TestFilterFBC_HeadsOnlyDepsFromAllChannels verifies that in heads-only mode,
// dependencies of ALL channel heads are resolved (not just the default channel).
func TestFilterFBC_HeadsOnlyDepsFromAllChannels(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "my-op", DefaultChannel: "stable"},
			{Name: "dep-op"},
		},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v1.0.0"}},
			},
			{
				Name:    "preview",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v2.0.0"}},
			},
			{
				Name:    "stable",
				Package: "dep-op",
				Entries: []declcfg.ChannelEntry{{Name: "dep-op.v1.0.0"}},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image: "reg/my-op@sha256:100",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)},
				},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image: "reg/my-op@sha256:200",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)},
					// Only the preview bundle requires dep-op.
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"dep-op"}`)},
				},
			},
			{
				Name: "dep-op.v1.0.0", Package: "dep-op",
				Image: "reg/dep-op@sha256:d00",
				Properties: []property.Property{
					{Type: olmPackage, Value: json.RawMessage(`{"packageName":"dep-op","version":"1.0.0"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	// In heads-only mode, both channel heads are included. The preview head
	// (my-op.v2.0.0) requires dep-op, so dep-op IS transitively included.
	pkgNames := map[string]bool{}
	for _, p := range filtered.Packages {
		pkgNames[p.Name] = true
	}
	if !pkgNames["dep-op"] {
		t.Error("dep-op SHOULD be included — the preview head requires it")
	}
	if !pkgNames["my-op"] {
		t.Error("my-op should be included")
	}
	if len(filtered.Packages) != 2 {
		t.Errorf("expected 2 packages (my-op + dep-op), got %d: %v", len(filtered.Packages), pkgNames)
	}
}

// TestFilterFBC_VersionFilterAllowsAllChannels verifies that when version
// filters are specified without explicit channels, all channels are searched.
func TestFilterFBC_VersionFilterAllowsAllChannels(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "my-op", DefaultChannel: "stable"},
		},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v1.0.0"}},
			},
			{
				Name:    "preview",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{{Name: "my-op.v2.0.0"}},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:100",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)}},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:200",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)}},
			},
		},
	}

	resolver := &CatalogResolver{}
	// MinVersion filter with no channels → searches all channels.
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.5.0"}},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	// Only 'preview' channel has a bundle matching >= 1.5.0.
	// 'stable' channel has only v1.0.0 and should be excluded.
	if len(filtered.Channels) != 1 {
		t.Errorf("expected 1 channel (matching version filter), got %d", len(filtered.Channels))
	}
	// Only v2.0.0 matches >= 1.5.0.
	if len(filtered.Bundles) != 1 {
		t.Errorf("expected 1 bundle (v2.0.0 >= 1.5.0), got %d", len(filtered.Bundles))
	}
}

// TestChannelHeadPlusN directly tests the channelHeadPlusN helper.
func TestChannelHeadPlusN(t *testing.T) {
	ch := declcfg.Channel{
		Name:    "stable",
		Package: "my-op",
		Entries: []declcfg.ChannelEntry{
			{Name: "my-op.v1.0.0"},
			{Name: "my-op.v2.0.0", Replaces: "my-op.v1.0.0"},
			{Name: "my-op.v3.0.0", Replaces: "my-op.v2.0.0"},
			{Name: "my-op.v4.0.0", Replaces: "my-op.v3.0.0"},
		},
	}

	tests := []struct {
		name     string
		previous int
		want     []string
	}{
		{"head only", 0, []string{"my-op.v4.0.0"}},
		{"head+1", 1, []string{"my-op.v3.0.0", "my-op.v4.0.0"}},
		{"head+2", 2, []string{"my-op.v2.0.0", "my-op.v3.0.0", "my-op.v4.0.0"}},
		{"head+10 (more than exist)", 10, []string{"my-op.v1.0.0", "my-op.v2.0.0", "my-op.v3.0.0", "my-op.v4.0.0"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := channelHeadPlusN(ch, tt.previous)
			if len(got) != len(tt.want) {
				t.Fatalf("channelHeadPlusN(prev=%d) = %v, want %v", tt.previous, got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("channelHeadPlusN(prev=%d)[%d] = %q, want %q", tt.previous, i, g, tt.want[i])
				}
			}
		})
	}
}

// TestChannelHeadPlusN_Skips verifies that Skips are considered when finding heads.
func TestChannelHeadPlusN_Skips(t *testing.T) {
	ch := declcfg.Channel{
		Name:    "stable",
		Package: "my-op",
		Entries: []declcfg.ChannelEntry{
			{Name: "my-op.v1.0.0"},
			{Name: "my-op.v2.0.0", Replaces: "my-op.v1.0.0"},
			// v3 skips v2 and replaces v1 → v2 is superseded by Skips.
			{Name: "my-op.v3.0.0", Replaces: "my-op.v1.0.0", Skips: []string{"my-op.v2.0.0"}},
		},
	}

	got := channelHeadPlusN(ch, 0)
	if len(got) != 1 || got[0] != "my-op.v3.0.0" {
		t.Errorf("expected [my-op.v3.0.0] as sole head, got %v", got)
	}

	// head+1: walk back via Replaces (v3 → v1), so we get v1 + v3.
	got = channelHeadPlusN(ch, 1)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries for head+1, got %v", got)
	}
	if got[0] != "my-op.v1.0.0" || got[1] != "my-op.v3.0.0" {
		t.Errorf("expected [my-op.v1.0.0, my-op.v3.0.0], got %v", got)
	}
}

// TestFilterFBC_HeadPlusN verifies the PreviousVersions field.
func TestFilterFBC_HeadPlusN(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "my-op", DefaultChannel: "stable"},
		},
		Channels: []declcfg.Channel{
			{
				Name:    "stable",
				Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0", Replaces: "my-op.v1.0.0"},
					{Name: "my-op.v3.0.0", Replaces: "my-op.v2.0.0"},
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "my-op.v1.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:100",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)}},
			},
			{
				Name: "my-op.v2.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:200",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"2.0.0"}`)}},
			},
			{
				Name: "my-op.v3.0.0", Package: "my-op",
				Image:      "reg/my-op@sha256:300",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"3.0.0"}`)}},
			},
		},
	}

	resolver := &CatalogResolver{}

	// head+0: only v3.0.0
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC head+0: %v", err)
	}
	if len(filtered.Bundles) != 1 {
		names := bundleNameSet(filtered.Bundles)
		t.Errorf("head+0: expected 1 bundle (v3), got %d: %v", len(filtered.Bundles), names)
	}

	// head+1: v2.0.0 + v3.0.0
	filtered, err = resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", PreviousVersions: 1},
	})
	if err != nil {
		t.Fatalf("FilterFBC head+1: %v", err)
	}
	names := bundleNameSet(filtered.Bundles)
	if len(filtered.Bundles) != 2 {
		t.Errorf("head+1: expected 2 bundles (v2+v3), got %d: %v", len(filtered.Bundles), names)
	}
	if !names["my-op.v2.0.0"] || !names["my-op.v3.0.0"] {
		t.Errorf("head+1: expected v2+v3, got %v", names)
	}

	// head+5: all 3 (more than chain length)
	filtered, err = resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op", PreviousVersions: 5},
	})
	if err != nil {
		t.Fatalf("FilterFBC head+5: %v", err)
	}
	if len(filtered.Bundles) != 3 {
		names = bundleNameSet(filtered.Bundles)
		t.Errorf("head+5: expected 3 bundles (all), got %d: %v", len(filtered.Bundles), names)
	}
}

func bundleNameSet(bundles []declcfg.Bundle) map[string]bool {
	m := make(map[string]bool, len(bundles))
	for _, b := range bundles {
		m[b.Name] = true
	}
	return m
}
