package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const opV2 = "op.v2.0.0"

func sbBundle(pkg, version string, extra ...property.Property) declcfg.Bundle {
	props := append([]property.Property{{
		Type:  olmPackage,
		Value: json.RawMessage(fmt.Sprintf(`{"packageName":%q,"version":%q}`, pkg, version)),
	}}, extra...)
	return declcfg.Bundle{
		Name: pkg + ".v" + version, Package: pkg,
		Image: "reg/" + pkg + "@sha256:" + strings.ReplaceAll(version, ".", ""), Properties: props,
	}
}

// op has v1.0.0 → v1.1.0 → v1.2.0 → v2.0.0 in "stable" and v1.2.0 in
// "fast"; v1.1.0 requires the "dep" package.
func selectedBundlesCatalog() *declcfg.DeclarativeConfig {
	depReq := property.Property{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"dep","versionRange":">=0.0.0"}`)}
	return &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op", DefaultChannel: "stable"}, {Name: "dep", DefaultChannel: "stable"}},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "op", Entries: []declcfg.ChannelEntry{
				{Name: "op.v1.0.0"},
				{Name: "op.v1.1.0", Replaces: "op.v1.0.0"},
				{Name: "op.v1.2.0", Replaces: "op.v1.1.0"},
				{Name: opV2, Replaces: "op.v1.2.0"},
			}},
			{Name: "fast", Package: "op", Entries: []declcfg.ChannelEntry{{Name: "op.v1.2.0"}}},
			{Name: "stable", Package: "dep", Entries: []declcfg.ChannelEntry{{Name: "dep.v0.1.0"}}},
		},
		Bundles: []declcfg.Bundle{
			sbBundle("op", "1.0.0"), sbBundle("op", "1.1.0", depReq), sbBundle("op", "1.2.0"), sbBundle("op", "2.0.0"),
			sbBundle("dep", "0.1.0"),
		},
	}
}

func TestFilterFBC_SelectedBundles(t *testing.T) {
	r := &CatalogResolver{}
	filtered, err := r.FilterFBC(context.Background(), selectedBundlesCatalog(), []mirrorv1alpha1.IncludePackage{{
		Name:    "op",
		Bundles: []mirrorv1alpha1.SelectedBundle{{Name: "op.v1.0.0"}, {Name: "op.v1.2.0"}},
	}})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}
	got := bundleNameSet(filtered.Bundles)
	for _, want := range []string{"op.v1.0.0", "op.v1.2.0"} {
		if !got[want] {
			t.Errorf("%s missing", want)
		}
	}
	for _, unwanted := range []string{"op.v1.1.0", opV2, "dep.v0.1.0"} {
		if got[unwanted] {
			t.Errorf("%s must not be selected", unwanted)
		}
	}

	channels := map[string]declcfg.Channel{}
	for _, ch := range filtered.Channels {
		channels[ch.Package+"/"+ch.Name] = ch
	}
	stable, ok := channels["op/stable"]
	if !ok || len(stable.Entries) != 2 {
		t.Fatalf("op/stable = %+v", stable)
	}
	if heads := channelHeads(stable); len(heads) != 1 || heads[0] != "op.v1.2.0" {
		t.Fatalf("stable heads = %v", heads)
	}
	// v1.2.0 now replaces v1.0.0 and skips the dropped v1.1.0.
	for _, e := range stable.Entries {
		if e.Name == "op.v1.2.0" && (e.Replaces != "op.v1.0.0" || len(e.Skips) != 1 || e.Skips[0] != "op.v1.1.0") {
			t.Fatalf("v1.2.0 entry = %+v", e)
		}
	}
	if fast, ok := channels["op/fast"]; !ok || len(fast.Entries) != 1 {
		t.Fatalf("op/fast = %+v", fast)
	}
}

func TestFilterFBC_SelectedBundlesPullDependencies(t *testing.T) {
	r := &CatalogResolver{}
	filtered, err := r.FilterFBC(context.Background(), selectedBundlesCatalog(), []mirrorv1alpha1.IncludePackage{{
		Name:    "op",
		Bundles: []mirrorv1alpha1.SelectedBundle{{Name: "op.v1.1.0"}},
	}})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}
	if got := bundleNameSet(filtered.Bundles); !got["dep.v0.1.0"] || !got["op.v1.1.0"] || len(got) != 2 {
		t.Fatalf("bundles = %v", got)
	}
}

func TestFilterFBC_SelectedBundleErrors(t *testing.T) {
	r := &CatalogResolver{}
	for name, bundle := range map[string]string{
		"unknown bundle":        "op.v9.9.9",
		"bundle of another pkg": "dep.v0.1.0",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.FilterFBC(context.Background(), selectedBundlesCatalog(), []mirrorv1alpha1.IncludePackage{{
				Name: "op", Bundles: []mirrorv1alpha1.SelectedBundle{{Name: bundle}},
			}})
			if err == nil || !strings.Contains(err.Error(), bundle) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Bundles on separate branches of a channel (not on one replaces chain) are
// joined under the highest one so the channel keeps a single head.
func TestFilterFBC_SelectedBundlesSingleHead(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op", DefaultChannel: "stable"}},
		Channels: []declcfg.Channel{{Name: "stable", Package: "op", Entries: []declcfg.ChannelEntry{
			{Name: "op.v1.0.0"},
			{Name: "op.v1.1.0", Skips: []string{"op.v1.0.0"}},
			{Name: opV2},
		}}},
		Bundles: []declcfg.Bundle{sbBundle("op", "1.0.0"), sbBundle("op", "1.1.0"), sbBundle("op", "2.0.0")},
	}
	r := &CatalogResolver{}
	filtered, err := r.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{{
		Name:    "op",
		Bundles: []mirrorv1alpha1.SelectedBundle{{Name: "op.v1.0.0"}, {Name: opV2}},
	}})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}
	if heads := channelHeads(filtered.Channels[0]); len(heads) != 1 || heads[0] != opV2 {
		t.Fatalf("heads = %v, entries = %+v", heads, filtered.Channels[0].Entries)
	}
}

func TestMergeChannelHeads_SingleHeadUnchanged(t *testing.T) {
	entries := []declcfg.ChannelEntry{{Name: "a"}, {Name: "b", Replaces: "a"}}
	if got := mergeChannelHeads(entries, nil); len(got[1].Skips) != 0 {
		t.Fatalf("entries changed: %+v", got)
	}
}
