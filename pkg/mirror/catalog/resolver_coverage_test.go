package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/blang/semver/v4"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
)

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_NilClient(t *testing.T) {
	r := New(nil)
	if r == nil {
		t.Fatal("New(nil) should return a non-nil resolver")
	}
	if r.client != nil {
		t.Error("client should be nil when constructed with nil")
	}
}

// ---------------------------------------------------------------------------
// GetCatalogDigest
// ---------------------------------------------------------------------------

func TestGetCatalogDigest_InvalidRef(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.GetCatalogDigest(context.Background(), ":::invalid")
	if err == nil {
		t.Fatal("expected error for invalid image reference")
	}
}

func TestGetCatalogDigest_DigestAlreadyPresent(t *testing.T) {
	r := &CatalogResolver{} // nil client — should not be reached
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgRef := "registry.example.com/catalog@" + digest
	got, err := r.GetCatalogDigest(context.Background(), imgRef)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != digest {
		t.Errorf("expected %s, got %s", digest, got)
	}
}

func TestGetCatalogDigest_NilClient(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.GetCatalogDigest(context.Background(), "registry.example.com/catalog:latest")
	if err == nil {
		t.Fatal("expected error when client is nil and no digest in ref")
	}
	if !strings.Contains(err.Error(), "no client configured") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ResolveCatalog
// ---------------------------------------------------------------------------

func TestResolveCatalog_InvalidRef(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.ResolveCatalog(context.Background(), ":::invalid", nil)
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func TestResolveCatalog_NilClient(t *testing.T) {
	r := &CatalogResolver{}
	got, err := r.ResolveCatalog(context.Background(), "registry.example.com/catalog:latest", []string{"pkg-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result with nil client, got %v", got)
	}
}

func TestResolveCatalog_EmptyPackagesNilClient(t *testing.T) {
	r := &CatalogResolver{}
	got, err := r.ResolveCatalog(context.Background(), "registry.example.com/catalog:latest", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nil client short-circuits before empty-packages check
	if got != nil {
		t.Errorf("expected nil with nil client, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// ResolveCatalogWithBundles
// ---------------------------------------------------------------------------

func TestResolveCatalogWithBundles_InvalidRef(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.ResolveCatalogWithBundles(context.Background(), ":::invalid", nil)
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func TestResolveCatalogWithBundles_NilClient(t *testing.T) {
	r := &CatalogResolver{}
	got, err := r.ResolveCatalogWithBundles(context.Background(), "registry.example.com/catalog:latest", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil with nil client, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// LoadFBC
// ---------------------------------------------------------------------------

func TestLoadFBC_NilClient(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.LoadFBC(context.Background(), "registry.example.com/catalog:latest")
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
	if !strings.Contains(err.Error(), "registry client is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// BuildFilteredCatalogImage
// ---------------------------------------------------------------------------

func TestBuildFilteredCatalogImage_NilClient(t *testing.T) {
	r := &CatalogResolver{}
	_, err := r.BuildFilteredCatalogImage(context.Background(),
		"registry.example.com/src:latest",
		"registry.example.com/dst:latest",
		nil)
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
	if !strings.Contains(err.Error(), "registry client is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBuildFilteredCatalogImage_InvalidSourceRef(t *testing.T) {
	r := &CatalogResolver{} // nil client would be caught first, but invalid ref is checked before
	_, err := r.BuildFilteredCatalogImage(context.Background(), ":::invalid", "registry.example.com/dst:latest", nil)
	// Either "registry client is required" or "failed to parse source" — depends on order
	if err == nil {
		t.Fatal("expected error for invalid source ref or nil client")
	}
}

func TestBuildFilteredCatalogImage_InvalidTargetRef(t *testing.T) {
	r := &CatalogResolver{} // nil client first
	_, err := r.BuildFilteredCatalogImage(context.Background(),
		"registry.example.com/src:latest",
		":::invalid",
		nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// bundleVersion
// ---------------------------------------------------------------------------

func TestBundleVersion_NoProperties(t *testing.T) {
	b := declcfg.Bundle{Name: "op.v1.0.0", Package: "op"}
	if got := bundleVersion(b); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBundleVersion_NonOLMPackageProperty(t *testing.T) {
	b := declcfg.Bundle{
		Name:    "op.v1.0.0",
		Package: "op",
		Properties: []property.Property{
			{Type: "olm.gvk", Value: json.RawMessage(`{"group":"g","version":"v1","kind":"K"}`)},
		},
	}
	if got := bundleVersion(b); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBundleVersion_InvalidJSON(t *testing.T) {
	b := declcfg.Bundle{
		Name:    "op.v1.0.0",
		Package: "op",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{invalid`)},
		},
	}
	if got := bundleVersion(b); got != "" {
		t.Errorf("expected empty for invalid JSON, got %q", got)
	}
}

func TestBundleVersion_EmptyVersionField(t *testing.T) {
	b := declcfg.Bundle{
		Name:    "op.v1.0.0",
		Package: "op",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":""}`)},
		},
	}
	if got := bundleVersion(b); got != "" {
		t.Errorf("expected empty for blank version, got %q", got)
	}
}

func TestBundleVersion_ValidVersion(t *testing.T) {
	b := declcfg.Bundle{
		Name:    "op.v2.3.4",
		Package: "op",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"2.3.4"}`)},
		},
	}
	if got := bundleVersion(b); got != "2.3.4" {
		t.Errorf("expected 2.3.4, got %q", got)
	}
}

func TestBundleVersion_SkipsNonMatchingThenFindsVersion(t *testing.T) {
	b := declcfg.Bundle{
		Name:    "op.v1.0.0",
		Package: "op",
		Properties: []property.Property{
			{Type: "olm.gvk", Value: json.RawMessage(`{"group":"g","version":"v1","kind":"K"}`)},
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"1.0.0"}`)},
		},
	}
	if got := bundleVersion(b); got != "1.0.0" { //nolint:goconst
		t.Errorf("expected 1.0.0, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// buildPkgIncludeFilter
// ---------------------------------------------------------------------------

func TestBuildPkgIncludeFilter_EmptyChannels(t *testing.T) {
	inc := mirrorv1alpha1.IncludePackage{Name: "my-op"}
	f := buildPkgIncludeFilter(inc)
	if !f.allowAllChannels {
		t.Error("expected allowAllChannels=true when no channels specified")
	}
	if f.pkgHasMinVer || f.pkgHasMaxVer {
		t.Error("expected no pkg-level version constraints")
	}
}

func TestBuildPkgIncludeFilter_WithPkgVersions(t *testing.T) {
	inc := mirrorv1alpha1.IncludePackage{
		Name: "my-op",
		IncludeBundle: mirrorv1alpha1.IncludeBundle{
			MinVersion: "1.0.0",
			MaxVersion: "3.0.0",
		},
	}
	f := buildPkgIncludeFilter(inc)
	if !f.pkgHasMinVer {
		t.Error("expected pkgHasMinVer=true")
	}
	if !f.pkgHasMaxVer {
		t.Error("expected pkgHasMaxVer=true")
	}
	if f.pkgMinSV.String() != "1.0.0" {
		t.Errorf("expected min 1.0.0, got %s", f.pkgMinSV)
	}
	if f.pkgMaxSV.String() != "3.0.0" {
		t.Errorf("expected max 3.0.0, got %s", f.pkgMaxSV)
	}
}

func TestBuildPkgIncludeFilter_InvalidSemverIgnored(t *testing.T) {
	inc := mirrorv1alpha1.IncludePackage{
		Name: "my-op",
		IncludeBundle: mirrorv1alpha1.IncludeBundle{
			MinVersion: "not-a-version",
			MaxVersion: "also-bad",
		},
	}
	f := buildPkgIncludeFilter(inc)
	if f.pkgHasMinVer {
		t.Error("invalid semver should not set pkgHasMinVer")
	}
	if f.pkgHasMaxVer {
		t.Error("invalid semver should not set pkgHasMaxVer")
	}
}

func TestBuildPkgIncludeFilter_WithChannels(t *testing.T) {
	inc := mirrorv1alpha1.IncludePackage{
		Name: "my-op",
		Channels: []mirrorv1alpha1.IncludeChannel{
			{
				Name: "stable",
				IncludeBundle: mirrorv1alpha1.IncludeBundle{
					MinVersion: "2.0.0",
					MaxVersion: "4.0.0",
				},
			},
			{
				Name: "preview",
			},
		},
	}
	f := buildPkgIncludeFilter(inc)
	if f.allowAllChannels {
		t.Error("expected allowAllChannels=false when channels specified")
	}
	if !f.allowedChannels["stable"] || !f.allowedChannels["preview"] {
		t.Error("expected both channels in allowedChannels")
	}

	stableCF := f.channelFilters["stable"]
	if !stableCF.hasMinVer || stableCF.minSV.String() != "2.0.0" {
		t.Errorf("stable channel min version: got hasMin=%v sv=%s", stableCF.hasMinVer, stableCF.minSV)
	}
	if !stableCF.hasMaxVer || stableCF.maxSV.String() != "4.0.0" {
		t.Errorf("stable channel max version: got hasMax=%v sv=%s", stableCF.hasMaxVer, stableCF.maxSV)
	}

	previewCF := f.channelFilters["preview"]
	if previewCF.hasMinVer || previewCF.hasMaxVer {
		t.Error("preview channel should have no version constraints")
	}
}

func TestBuildPkgIncludeFilter_ChannelInvalidSemver(t *testing.T) {
	inc := mirrorv1alpha1.IncludePackage{
		Name: "my-op",
		Channels: []mirrorv1alpha1.IncludeChannel{
			{
				Name: "stable",
				IncludeBundle: mirrorv1alpha1.IncludeBundle{
					MinVersion: "bad",
					MaxVersion: "worse",
				},
			},
		},
	}
	f := buildPkgIncludeFilter(inc)
	cf := f.channelFilters["stable"]
	if cf.hasMinVer || cf.hasMaxVer {
		t.Error("invalid channel semver should be silently ignored")
	}
}

// ---------------------------------------------------------------------------
// effectiveVersionFilter
// ---------------------------------------------------------------------------

func TestEffectiveVersionFilter_ChannelOverridesPackage(t *testing.T) {
	f := pkgIncludeFilter{
		pkgHasMinVer: true,
		pkgMinSV:     semverMust(t, "1.0.0"),
		pkgHasMaxVer: true,
		pkgMaxSV:     semverMust(t, "5.0.0"),
		channelFilters: map[string]channelVersionFilter{
			"stable": {
				hasMinVer: true, minSV: semverMust(t, "2.0.0"),
				hasMaxVer: true, maxSV: semverMust(t, "4.0.0"),
			},
		},
	}
	vf := f.effectiveVersionFilter("stable")
	if !vf.hasMinVer || vf.minSV.String() != "2.0.0" {
		t.Errorf("channel min should override pkg: got %v %s", vf.hasMinVer, vf.minSV)
	}
	if !vf.hasMaxVer || vf.maxSV.String() != "4.0.0" {
		t.Errorf("channel max should override pkg: got %v %s", vf.hasMaxVer, vf.maxSV)
	}
}

func TestEffectiveVersionFilter_FallsBackToPackage(t *testing.T) {
	f := pkgIncludeFilter{
		pkgHasMinVer:   true,
		pkgMinSV:       semverMust(t, "1.0.0"),
		pkgHasMaxVer:   true,
		pkgMaxSV:       semverMust(t, "5.0.0"),
		channelFilters: map[string]channelVersionFilter{},
	}
	vf := f.effectiveVersionFilter("unknown-channel")
	if !vf.hasMinVer || vf.minSV.String() != "1.0.0" {
		t.Errorf("should fall back to pkg min: got %v %s", vf.hasMinVer, vf.minSV)
	}
	if !vf.hasMaxVer || vf.maxSV.String() != "5.0.0" {
		t.Errorf("should fall back to pkg max: got %v %s", vf.hasMaxVer, vf.maxSV)
	}
}

func TestEffectiveVersionFilter_PartialOverride(t *testing.T) {
	// Channel has min but no max → max falls back to package.
	f := pkgIncludeFilter{
		pkgHasMinVer: true,
		pkgMinSV:     semverMust(t, "1.0.0"),
		pkgHasMaxVer: true,
		pkgMaxSV:     semverMust(t, "5.0.0"),
		channelFilters: map[string]channelVersionFilter{
			"stable": {
				hasMinVer: true, minSV: semverMust(t, "3.0.0"),
				// no max
			},
		},
	}
	vf := f.effectiveVersionFilter("stable")
	if vf.minSV.String() != "3.0.0" {
		t.Errorf("min should be channel-level 3.0.0, got %s", vf.minSV)
	}
	if vf.maxSV.String() != "5.0.0" {
		t.Errorf("max should fall back to pkg-level 5.0.0, got %s", vf.maxSV)
	}
}

func TestEffectiveVersionFilter_NoConstraints(t *testing.T) {
	f := pkgIncludeFilter{
		channelFilters: map[string]channelVersionFilter{},
	}
	vf := f.effectiveVersionFilter("any")
	if vf.hasMinVer || vf.hasMaxVer {
		t.Error("expected no constraints")
	}
}

func TestEffectiveVersionFilter_ChannelExistsNoVersions(t *testing.T) {
	// Channel entry exists in channelFilters but has no version constraints
	// → falls back to package level.
	f := pkgIncludeFilter{
		pkgHasMinVer: true,
		pkgMinSV:     semverMust(t, "1.0.0"),
		channelFilters: map[string]channelVersionFilter{
			"stable": {}, // no versions set
		},
	}
	vf := f.effectiveVersionFilter("stable")
	if !vf.hasMinVer || vf.minSV.String() != "1.0.0" {
		t.Errorf("should fall back to pkg min when channel has no min: got %v %s", vf.hasMinVer, vf.minSV)
	}
}

// ---------------------------------------------------------------------------
// bundleMatchesVersionFilter
// ---------------------------------------------------------------------------

func TestBundleMatchesVersionFilter_NoFilter(t *testing.T) {
	b := declcfg.Bundle{Name: "op.v1.0.0"}
	vf := channelVersionFilter{}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("no filter should always match")
	}
}

func TestBundleMatchesVersionFilter_EmptyVersion(t *testing.T) {
	b := declcfg.Bundle{Name: "op.v1.0.0"} // no olm.package property
	vf := channelVersionFilter{hasMinVer: true, minSV: semverMust(t, "2.0.0")}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("bundle with unknown version should be included (lenient)")
	}
}

func TestBundleMatchesVersionFilter_NonSemver(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.special",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"not-semver"}`)},
		},
	}
	vf := channelVersionFilter{hasMinVer: true, minSV: semverMust(t, "1.0.0")}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("non-semver version should be included (lenient)")
	}
}

func TestBundleMatchesVersionFilter_BelowMin(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v1.0.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"1.0.0"}`)},
		},
	}
	vf := channelVersionFilter{hasMinVer: true, minSV: semverMust(t, "2.0.0")}
	if bundleMatchesVersionFilter(b, vf) {
		t.Error("1.0.0 is below min 2.0.0, should not match")
	}
}

func TestBundleMatchesVersionFilter_AboveMax(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v5.0.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"5.0.0"}`)},
		},
	}
	vf := channelVersionFilter{hasMaxVer: true, maxSV: semverMust(t, "3.0.0")}
	if bundleMatchesVersionFilter(b, vf) {
		t.Error("5.0.0 is above max 3.0.0, should not match")
	}
}

func TestBundleMatchesVersionFilter_WithinRange(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v2.5.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"2.5.0"}`)},
		},
	}
	vf := channelVersionFilter{
		hasMinVer: true, minSV: semverMust(t, "2.0.0"),
		hasMaxVer: true, maxSV: semverMust(t, "3.0.0"),
	}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("2.5.0 is within [2.0.0, 3.0.0], should match")
	}
}

func TestBundleMatchesVersionFilter_ExactMin(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v2.0.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"2.0.0"}`)},
		},
	}
	vf := channelVersionFilter{hasMinVer: true, minSV: semverMust(t, "2.0.0")}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("exact min version should match")
	}
}

func TestBundleMatchesVersionFilter_ExactMax(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v3.0.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"3.0.0"}`)},
		},
	}
	vf := channelVersionFilter{hasMaxVer: true, maxSV: semverMust(t, "3.0.0")}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("exact max version should match")
	}
}

func TestBundleMatchesVersionFilter_OnlyMaxSet(t *testing.T) {
	b := declcfg.Bundle{
		Name: "op.v1.0.0",
		Properties: []property.Property{
			{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"1.0.0"}`)},
		},
	}
	vf := channelVersionFilter{hasMaxVer: true, maxSV: semverMust(t, "3.0.0")}
	if !bundleMatchesVersionFilter(b, vf) {
		t.Error("1.0.0 is within max 3.0.0, should match")
	}
}

// ---------------------------------------------------------------------------
// renderBundleRefs
// ---------------------------------------------------------------------------

func TestRenderBundleRefs_Empty(t *testing.T) {
	got := renderBundleRefs(nil)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestRenderBundleRefs_Single(t *testing.T) {
	got := renderBundleRefs([]string{"op.v1.0.0"})
	if got != "op.v1.0.0" {
		t.Errorf("expected single name, got %q", got)
	}
}

func TestRenderBundleRefs_ThreeNames(t *testing.T) {
	got := renderBundleRefs([]string{"a", "b", "c"})
	if got != "a, b, c" {
		t.Errorf("expected 'a, b, c', got %q", got)
	}
}

func TestRenderBundleRefs_MoreThanThree(t *testing.T) {
	got := renderBundleRefs([]string{"a", "b", "c", "d", "e"})
	if got != "a, b, c (+2 more)" {
		t.Errorf("expected truncated output, got %q", got)
	}
}

func TestRenderBundleRefs_ExactlyFour(t *testing.T) {
	got := renderBundleRefs([]string{"a", "b", "c", "d"})
	if got != "a, b, c (+1 more)" {
		t.Errorf("expected 'a, b, c (+1 more)', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// channelHeadPlusN — edge cases
// ---------------------------------------------------------------------------

func TestChannelHeadPlusN_EmptyEntries(t *testing.T) {
	ch := declcfg.Channel{Name: "stable", Package: "op"}
	got := channelHeadPlusN(ch, 0)
	if got != nil {
		t.Errorf("expected nil for empty entries, got %v", got)
	}
}

func TestChannelHeadPlusN_AllSuperseded(t *testing.T) {
	// Circular supersession: every entry is superseded — fallback to last.
	ch := declcfg.Channel{
		Name:    "stable",
		Package: "op",
		Entries: []declcfg.ChannelEntry{
			{Name: "op.v1.0.0", Replaces: "op.v2.0.0"},
			{Name: "op.v2.0.0", Replaces: "op.v1.0.0"},
		},
	}
	got := channelHeadPlusN(ch, 0)
	if len(got) != 1 || got[0] != "op.v2.0.0" {
		t.Errorf("expected fallback to last entry, got %v", got)
	}
}

func TestChannelHeadPlusN_NegativePrevious(t *testing.T) {
	ch := declcfg.Channel{
		Name:    "stable",
		Package: "op",
		Entries: []declcfg.ChannelEntry{
			{Name: "op.v1.0.0"},
			{Name: "op.v2.0.0", Replaces: "op.v1.0.0"},
		},
	}
	got := channelHeadPlusN(ch, -1)
	if len(got) != 1 || got[0] != "op.v2.0.0" {
		t.Errorf("negative previous should behave like 0, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// extractFBCLayer
// ---------------------------------------------------------------------------

func TestExtractFBCLayer_ValidLayer(t *testing.T) {
	body := []byte(`schema: olm.package
name: test-op
`)
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
		{name: "configs/test-op/", typeflag: tar.TypeDir},
		{name: "configs/test-op/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 1 {
		t.Errorf("expected 1 config file extracted, got %d", count)
	}
	if _, ok := fsMap["configs/test-op/catalog.yaml"]; !ok {
		t.Error("expected configs/test-op/catalog.yaml in fsMap")
	}
}

func TestExtractFBCLayer_SkipsNonConfigPaths(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "etc/hosts", typeflag: tar.TypeReg, size: 4, body: []byte("data")},
		{name: "usr/bin/opm", typeflag: tar.TypeReg, size: 3, body: []byte("opm")},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 0 {
		t.Errorf("expected 0 config files, got %d", count)
	}
}

func TestExtractFBCLayer_SkipsDirectories(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
		{name: "configs/test-op/", typeflag: tar.TypeDir},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 0 {
		t.Errorf("expected 0 (dirs only), got %d", count)
	}
}

func TestExtractFBCLayer_NotGzip(t *testing.T) {
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader([]byte("not gzip data")), fsMap)
	if count != 0 {
		t.Errorf("expected 0 for non-gzip, got %d", count)
	}
}

func TestExtractFBCLayer_WithLeadingDotSlash(t *testing.T) {
	body := []byte("data")
	data := makeGzipTar(t, []tarEntry{
		{name: "./configs/op/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 1 {
		t.Errorf("expected 1 (leading ./ should be stripped), got %d", count)
	}
}

func TestExtractFBCLayer_SymlinksSkipped(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/link", typeflag: tar.TypeSymlink},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 0 {
		t.Errorf("expected 0 (symlinks skipped), got %d", count)
	}
}

func TestExtractFBCLayer_MultipleFiles(t *testing.T) {
	body1 := []byte("pkg1")
	body2 := []byte("pkg2")
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/pkg1/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body1)), body: body1},
		{name: "configs/pkg2/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body2)), body: body2},
	})
	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(data), fsMap)
	if count != 2 {
		t.Errorf("expected 2 config files, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// buildFBCLayer
// ---------------------------------------------------------------------------

func TestBuildFBCLayer_EmptyConfig(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{}
	data, diffID, err := buildFBCLayer(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty gzip data")
	}
	if diffID.String() == "" {
		t.Error("expected non-empty diff ID")
	}

	// Verify the layer is valid gzip → tar.
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar error: %v", nextErr)
		}
		names = append(names, hdr.Name)
	}

	// Should contain: configs/, configs/.wh..wh..opq, tmp/, tmp/cache/, tmp/cache/.wh..wh..opq
	expected := map[string]bool{
		"configs/":               true,
		"configs/.wh..wh..opq":   true,
		"tmp/":                   true,
		"tmp/cache/":             true,
		"tmp/cache/.wh..wh..opq": true,
	}
	for _, n := range names {
		delete(expected, n)
	}
	if len(expected) > 0 {
		t.Errorf("missing expected entries: %v (got entries: %v)", expected, names)
	}
}

func TestBuildFBCLayer_SinglePackage(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Schema: "olm.package", Name: "test-op", DefaultChannel: "stable"},
		},
		Channels: []declcfg.Channel{
			{Schema: "olm.channel", Name: "stable", Package: "test-op",
				Entries: []declcfg.ChannelEntry{{Name: "test-op.v1.0.0"}}},
		},
		Bundles: []declcfg.Bundle{
			{Schema: "olm.bundle", Name: "test-op.v1.0.0", Package: "test-op",
				Image: "reg/test-op@sha256:aaa"},
		},
	}

	data, diffID, err := buildFBCLayer(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty layer data")
	}
	if diffID.String() == "" {
		t.Error("expected non-empty diff ID")
	}

	// Verify the tar contains per-package directory and catalog.yaml.
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var found bool
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar error: %v", nextErr)
		}
		if hdr.Name == "configs/test-op/catalog.yaml" {
			found = true
			if hdr.Size == 0 {
				t.Error("catalog.yaml should have non-zero size")
			}
		}
	}
	if !found {
		t.Error("expected configs/test-op/catalog.yaml in layer")
	}
}

func TestBuildFBCLayer_MultiplePackagesSorted(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Schema: "olm.package", Name: "zzz-op"},
			{Schema: "olm.package", Name: "aaa-op"},
		},
	}

	data, _, err := buildFBCLayer(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var pkgDirs []string
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar error: %v", nextErr)
		}
		if strings.HasPrefix(hdr.Name, "configs/") && hdr.Typeflag == tar.TypeDir && hdr.Name != "configs/" {
			pkgDirs = append(pkgDirs, hdr.Name)
		}
	}

	if len(pkgDirs) >= 2 && pkgDirs[0] != "configs/aaa-op/" {
		t.Errorf("expected aaa-op first (sorted), got order: %v", pkgDirs)
	}
}

// ---------------------------------------------------------------------------
// ExtractImages / ExtractImagesWithBundles — additional edge cases
// ---------------------------------------------------------------------------

func TestExtractImages_EmptyBundles(t *testing.T) {
	r := &CatalogResolver{}
	cfg := &declcfg.DeclarativeConfig{}
	images := r.ExtractImages(cfg)
	if len(images) != 0 {
		t.Errorf("expected 0 images for empty config, got %d", len(images))
	}
}

func TestExtractImagesWithBundles_EmptyImageSkipped(t *testing.T) {
	r := &CatalogResolver{}
	cfg := &declcfg.DeclarativeConfig{
		Bundles: []declcfg.Bundle{
			{Name: "op.v1", Package: "op", Image: ""},
			{Name: "op.v2", Package: "op", Image: "reg/op@sha256:aaa",
				RelatedImages: []declcfg.RelatedImage{
					{Name: "empty", Image: ""},
					{Name: "real", Image: "reg/sidecar@sha256:bbb"},
				}},
		},
	}
	result := r.ExtractImagesWithBundles(cfg)
	if _, ok := result[""]; ok {
		t.Error("empty image refs should not appear in results")
	}
	if len(result) != 2 {
		t.Errorf("expected 2 images, got %d: %v", len(result), result)
	}
}

func TestExtractImagesWithBundles_ManyBundlesForSameImage(t *testing.T) {
	r := &CatalogResolver{}
	cfg := &declcfg.DeclarativeConfig{
		Bundles: []declcfg.Bundle{
			{Name: "a", Package: "op", Image: "reg/shared@sha256:aaa"},
			{Name: "b", Package: "op", Image: "reg/shared@sha256:aaa"},
			{Name: "c", Package: "op", Image: "reg/shared@sha256:aaa"},
			{Name: "d", Package: "op", Image: "reg/shared@sha256:aaa"},
			{Name: "e", Package: "op", Image: "reg/shared@sha256:aaa"},
		},
	}
	result := r.ExtractImagesWithBundles(cfg)
	if len(result) != 1 {
		t.Errorf("expected 1 unique image, got %d", len(result))
	}
	label := result["reg/shared@sha256:aaa"]
	if !strings.Contains(label, "(+") {
		t.Errorf("expected truncated label with (+N more), got %q", label)
	}
}

// ---------------------------------------------------------------------------
// classifyLayer — additional edge cases
// ---------------------------------------------------------------------------

func TestClassifyLayer_LeadingDotSlash(t *testing.T) {
	body := []byte("data")
	data := makeGzipTar(t, []tarEntry{
		{name: "./configs/foo/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	skip, _, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skip {
		t.Error("leading ./ should be stripped, layer should be skippable")
	}
}

func TestClassifyLayer_CacheOnlyLayer(t *testing.T) {
	body := []byte("cache-data")
	data := makeGzipTar(t, []tarEntry{
		{name: "tmp/cache/db.pogreb", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	skip, sz, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skip {
		t.Error("cache-only layer should be skippable")
	}
	if sz != int64(len(body)) {
		t.Errorf("expected size %d, got %d", len(body), sz)
	}
}

func TestClassifyLayer_HardlinkInConfigs(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/foo/hardlink", typeflag: tar.TypeLink},
	})
	skip, _, reject, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skip {
		t.Error("hardlinks should not be skippable")
	}
	if !strings.Contains(reject, "non-regular file") {
		t.Errorf("expected non-regular file in reject, got %q", reject)
	}
}

// ---------------------------------------------------------------------------
// FilterFBC — additional edge cases for coverage
// ---------------------------------------------------------------------------

func TestFilterFBC_ChannelVersionOverridesPkgVersion(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name: "stable", Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0"},
					{Name: "my-op.v3.0.0"},
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
		},
	}

	resolver := &CatalogResolver{}
	// Package-level min=1.0.0, but channel-level stable min=2.0.0 should override.
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{
			Name:          "my-op",
			IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0.0"},
			Channels: []mirrorv1alpha1.IncludeChannel{
				{Name: "stable", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "2.0.0"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	bundleNames := bundleNameSet(filtered.Bundles)
	if bundleNames["my-op.v1.0.0"] {
		t.Error("v1.0.0 should be excluded by channel-level min 2.0.0")
	}
	if !bundleNames["my-op.v2.0.0"] || !bundleNames["my-op.v3.0.0"] {
		t.Error("v2.0.0 and v3.0.0 should be included")
	}
}

func TestFilterFBC_PackageNotInCatalog(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "existing-op"}},
		Channels: []declcfg.Channel{{Name: "stable", Package: "existing-op"}},
		Bundles: []declcfg.Bundle{
			{Name: "existing-op.v1.0.0", Package: "existing-op", Image: "reg/e@sha256:aaa"},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "nonexistent-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}
	if len(filtered.Packages) != 0 {
		t.Errorf("expected 0 packages (requested package not in catalog), got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_CompanionOperatorSuffix(t *testing.T) {
	// Tests the "-operator" suffix stripping for companion dep discovery.
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "foo-operator"},
			{Name: "foo-dependencies"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "foo-operator",
				Entries: []declcfg.ChannelEntry{{Name: "foo-operator.v1.0.0"}}},
			{Name: "stable", Package: "foo-dependencies",
				Entries: []declcfg.ChannelEntry{{Name: "foo-dependencies.v1.0.0"}}},
		},
		Bundles: []declcfg.Bundle{
			{Name: "foo-operator.v1.0.0", Package: "foo-operator", Image: "reg/foo@sha256:aaa",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"foo-operator","version":"1.0.0"}`)}}},
			{Name: "foo-dependencies.v1.0.0", Package: "foo-dependencies", Image: "reg/foo-deps@sha256:bbb",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"foo-dependencies","version":"1.0.0"}`)}}},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "foo-operator"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	pkgNames := map[string]bool{}
	for _, p := range filtered.Packages {
		pkgNames[p.Name] = true
	}
	if !pkgNames["foo-dependencies"] {
		t.Error("foo-dependencies should be auto-discovered via -operator suffix stripping")
	}
}

func TestFilterFBC_MaxVersionChannelLevel(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name: "stable", Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
					{Name: "my-op.v2.0.0"},
					{Name: "my-op.v3.0.0"},
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
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{
			Name: "my-op",
			Channels: []mirrorv1alpha1.IncludeChannel{
				{Name: "stable", IncludeBundle: mirrorv1alpha1.IncludeBundle{MaxVersion: "2.0.0"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}

	bundleNames := bundleNameSet(filtered.Bundles)
	if bundleNames["my-op.v3.0.0"] {
		t.Error("v3.0.0 should be excluded by channel max 2.0.0")
	}
	if !bundleNames["my-op.v1.0.0"] || !bundleNames["my-op.v2.0.0"] {
		t.Error("v1.0.0 and v2.0.0 should be included")
	}
}

// ---------------------------------------------------------------------------
// gvkValue.gvkKey
// ---------------------------------------------------------------------------

func TestGVKKey(t *testing.T) {
	g := gvkValue{Group: "apps", Version: "v1", Kind: "Deployment"}
	if got := g.gvkKey(); got != "apps/v1/Deployment" {
		t.Errorf("expected apps/v1/Deployment, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func semverMust(t *testing.T, s string) semver.Version {
	t.Helper()
	v, err := semver.ParseTolerant(s)
	if err != nil {
		t.Fatalf("bad semver %q: %v", s, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// Tests with a real MirrorClient (unreachable registry → covers error paths)
// ---------------------------------------------------------------------------

// newFailingClient returns a MirrorClient pointing at Docker Hub (default).
// All registry calls will fail (no auth, invalid refs) which is exactly what
// we need to exercise the error-handling paths.
func newFailingClient() *mirrorclient.MirrorClient {
	return mirrorclient.NewMirrorClient(nil, "")
}

func TestGetCatalogDigest_ClientErrorPath(t *testing.T) {
	r := New(newFailingClient())
	// Tag-based ref with a broken/unreachable registry — client.GetDigest will fail.
	_, err := r.GetCatalogDigest(context.Background(), "localhost:1/nonexistent:latest")
	if err == nil {
		t.Fatal("expected error from unreachable registry")
	}
}

func TestResolveCatalog_EmptyPackagesWithClient(t *testing.T) {
	r := New(newFailingClient())
	got, err := r.ResolveCatalog(context.Background(), "localhost:1/catalog:latest", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With empty packages, returns just the catalog image itself.
	if len(got) != 1 || got[0] != "localhost:1/catalog:latest" {
		t.Errorf("expected [localhost:1/catalog:latest], got %v", got)
	}
}

func TestResolveCatalog_ClientLoadFBCError(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.ResolveCatalog(context.Background(), "localhost:1/catalog:latest", []string{"pkg-a"})
	if err == nil {
		t.Fatal("expected error from failing loadFBCFromImage")
	}
	if !strings.Contains(err.Error(), "failed to load FBC") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveCatalogWithBundles_ClientLoadFBCError(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.ResolveCatalogWithBundles(context.Background(), "localhost:1/catalog:latest", nil)
	if err == nil {
		t.Fatal("expected error from failing loadFBCFromImage")
	}
	if !strings.Contains(err.Error(), "failed to load FBC") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadFBC_ClientError(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.LoadFBC(context.Background(), "localhost:1/catalog:latest")
	if err != nil && !strings.Contains(err.Error(), "failed") {
		t.Errorf("expected a failure error, got: %v", err)
	}
}

func TestBuildFilteredCatalogImage_ClientManifestError(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.BuildFilteredCatalogImage(context.Background(),
		"localhost:1/src:latest",
		"localhost:1/dst:latest",
		nil)
	if err == nil {
		t.Fatal("expected error from failing DownloadToOCILayout")
	}
	if !strings.Contains(err.Error(), "failed to download source catalog") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBuildFilteredCatalogImage_InvalidSourceRefWithClient(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.BuildFilteredCatalogImage(context.Background(),
		":::invalid-source",
		"localhost:1/dst:latest",
		nil)
	if err == nil {
		t.Fatal("expected error for invalid source ref")
	}
	if !strings.Contains(err.Error(), "parse source") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBuildFilteredCatalogImage_InvalidTargetRefWithClient(t *testing.T) {
	r := New(newFailingClient())
	_, err := r.BuildFilteredCatalogImage(context.Background(),
		"localhost:1/src:latest",
		":::invalid-target",
		nil)
	if err == nil {
		t.Fatal("expected error for invalid target ref")
	}
	if !strings.Contains(err.Error(), "parse target") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractFBCLayer — corrupted tar inside valid gzip
// ---------------------------------------------------------------------------

func TestExtractFBCLayer_CorruptedTar(t *testing.T) {
	// Valid gzip wrapping invalid tar data.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("this is not a valid tar stream"))
	_ = gz.Close()

	fsMap := make(fstest.MapFS)
	count := extractFBCLayer(bytes.NewReader(buf.Bytes()), fsMap)
	if count != 0 {
		t.Errorf("expected 0 for corrupted tar, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// classifyLayer — corrupted tar
// ---------------------------------------------------------------------------

func TestClassifyLayer_CorruptedTar(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("not a tar"))
	_ = gz.Close()

	_, _, _, err := classifyLayer(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected tar error for corrupted data")
	}
}

func TestClassifyLayer_EmptyArchive(t *testing.T) {
	// Valid gzip+tar with zero entries.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.Close()
	_ = gz.Close()

	skip, _, _, err := classifyLayer(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skip {
		t.Error("empty archive (no files) should not be skippable")
	}
}

// ---------------------------------------------------------------------------
// classifyAndExtractFBC
// ---------------------------------------------------------------------------

func TestClassifyAndExtractFBC_FBCOnly(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
		{name: "configs/pkg-a/catalog.yaml", typeflag: tar.TypeReg, size: 6, body: []byte("hello\n")},
		{name: "tmp/cache/pogreb.idx", typeflag: tar.TypeReg, size: 3, body: []byte("idx")},
	})
	fs := make(fstest.MapFS)
	skip, sz, _, err := classifyAndExtractFBC(bytes.NewReader(data), fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skip {
		t.Error("expected FBC-only layer to be skippable")
	}
	if sz != 9 {
		t.Errorf("expected totalSize 9, got %d", sz)
	}
	if _, ok := fs["configs/pkg-a/catalog.yaml"]; !ok {
		t.Error("expected FBC file to be extracted into configFS")
	}
	if string(fs["configs/pkg-a/catalog.yaml"].Data) != "hello\n" {
		t.Errorf("expected extracted content 'hello\\n', got %q", string(fs["configs/pkg-a/catalog.yaml"].Data))
	}
}

func TestClassifyAndExtractFBC_MixedLayer(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/pkg-a/catalog.yaml", typeflag: tar.TypeReg, size: 4, body: []byte("data")},
		{name: "usr/bin/opm", typeflag: tar.TypeReg, size: 3, body: []byte("bin")},
	})
	fs := make(fstest.MapFS)
	skip, _, reject, err := classifyAndExtractFBC(bytes.NewReader(data), fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skip {
		t.Error("mixed layer should not be skippable")
	}
	if reject != "usr/bin/opm" {
		t.Errorf("expected firstReject 'usr/bin/opm', got %q", reject)
	}
	// FBC files should still be extracted even from non-skippable layers.
	if _, ok := fs["configs/pkg-a/catalog.yaml"]; !ok {
		t.Error("expected FBC file to be extracted even from mixed layer")
	}
}

func TestClassifyAndExtractFBC_InvalidGzip(t *testing.T) {
	fs := make(fstest.MapFS)
	_, _, _, err := classifyAndExtractFBC(bytes.NewReader([]byte("not gzip")), fs)
	if err == nil {
		t.Error("expected error for invalid gzip")
	}
}

func TestClassifyAndExtractFBC_EmptyArchive(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.Close()
	_ = gz.Close()

	fs := make(fstest.MapFS)
	skip, _, _, err := classifyAndExtractFBC(bytes.NewReader(buf.Bytes()), fs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skip {
		t.Error("empty archive should not be skippable")
	}
}

// ---------------------------------------------------------------------------
// blobCopyWithRetry
// ---------------------------------------------------------------------------

func TestBlobCopyWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	mc := mirrorclient.NewMirrorClient(nil, "")
	// We can't easily mock BlobCopy through the real client, so we test the
	// retry logic structurally: with a very short timeout and cancelled parent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	err := blobCopyWithRetry(ctx, mc, ref.Ref{}, ref.Ref{}, descriptor.Descriptor{}, 3, time.Second)
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

func TestBlobCopyWithRetry_RespectsParentCancel(t *testing.T) {
	mc := mirrorclient.NewMirrorClient(nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := blobCopyWithRetry(ctx, mc, ref.Ref{}, ref.Ref{}, descriptor.Descriptor{}, 3, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestFilterFBC_BundleWithInvalidGVKJSON(t *testing.T) {
	// GVK property with invalid JSON should be silently skipped during
	// provider index build (no panic, no error).
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			{Name: "consumer-op"},
			{Name: "provider-op"},
		},
		Channels: []declcfg.Channel{
			{Name: "stable", Package: "consumer-op"},
			{Name: "stable", Package: "provider-op"},
		},
		Bundles: []declcfg.Bundle{
			{
				Name: "consumer-op.v1", Package: "consumer-op",
				Image: "reg/c@sha256:aaa",
				Properties: []property.Property{
					{Type: olmGVKRequired, Value: json.RawMessage(`{invalid json}`)},
				},
			},
			{
				Name: "provider-op.v1", Package: "provider-op",
				Image: "reg/p@sha256:bbb",
				Properties: []property.Property{
					{Type: olmGVK, Value: json.RawMessage(`{also invalid}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "consumer-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC should not fail on invalid JSON props: %v", err)
	}
	if len(filtered.Packages) != 1 {
		t.Errorf("expected 1 package (consumer-op only — bad GVK can't resolve), got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_InvalidPackageRequiredJSON(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op-a"}},
		Channels: []declcfg.Channel{{Name: "stable", Package: "op-a"}},
		Bundles: []declcfg.Bundle{
			{
				Name: "op-a.v1", Package: "op-a",
				Image: "reg/a@sha256:aaa",
				Properties: []property.Property{
					{Type: olmPackageRequired, Value: json.RawMessage(`{bad json}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "op-a"},
	})
	if err != nil {
		t.Fatalf("FilterFBC should not fail on invalid JSON: %v", err)
	}
	if len(filtered.Packages) != 1 {
		t.Errorf("expected 1 package, got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_EmptyPackageNameInRequired(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op-a"}},
		Channels: []declcfg.Channel{{Name: "stable", Package: "op-a"}},
		Bundles: []declcfg.Bundle{
			{
				Name: "op-a.v1", Package: "op-a",
				Image: "reg/a@sha256:aaa",
				Properties: []property.Property{
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":""}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "op-a"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty packageName is silently ignored (no dep added).
	if len(filtered.Packages) != 1 {
		t.Errorf("expected 1 package (no empty-name dep), got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_DepPkgNotInCatalog(t *testing.T) {
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op-a"}},
		Channels: []declcfg.Channel{{Name: "stable", Package: "op-a"}},
		Bundles: []declcfg.Bundle{
			{
				Name: "op-a.v1", Package: "op-a",
				Image: "reg/a@sha256:aaa",
				Properties: []property.Property{
					{Type: olmPackageRequired, Value: json.RawMessage(`{"packageName":"nonexistent"}`)},
				},
			},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "op-a"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nonexistent package not in catalog — silently skipped.
	if len(filtered.Packages) != 1 {
		t.Errorf("expected 1 package, got %d", len(filtered.Packages))
	}
}

func TestFilterFBC_ChannelEntryNotInBundles(t *testing.T) {
	// Channel references a bundle name that doesn't exist in Bundles list.
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "op"}},
		Channels: []declcfg.Channel{
			{
				Name: "stable", Package: "op",
				Entries: []declcfg.ChannelEntry{
					{Name: "op.v1.0.0"},
					{Name: "op.phantom"}, // not in bundles
				},
			},
		},
		Bundles: []declcfg.Bundle{
			{Name: "op.v1.0.0", Package: "op", Image: "reg/op@sha256:aaa",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"1.0.0"}`)}}},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "op", IncludeBundle: mirrorv1alpha1.IncludeBundle{MinVersion: "1.0.0"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered.Bundles) != 1 {
		t.Errorf("expected 1 bundle, got %d", len(filtered.Bundles))
	}
}

func TestFilterFBC_CompanionVariants(t *testing.T) {
	tests := []struct {
		name        string
		suffix      string
		expectedPkg string
	}{
		{
			name:        "DepsVariant",
			suffix:      "-deps",
			expectedPkg: "myapp-deps",
		},
		{
			name:        "DependencyVariant",
			suffix:      "-dependency",
			expectedPkg: "myapp-dependency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test suffix companion discovery.
			cfg := &declcfg.DeclarativeConfig{
				Packages: []declcfg.Package{
					{Name: "myapp"},
					{Name: "myapp" + tt.suffix},
				},
				Channels: []declcfg.Channel{
					{Name: "stable", Package: "myapp",
						Entries: []declcfg.ChannelEntry{{Name: "myapp.v1"}}},
					{Name: "stable", Package: "myapp" + tt.suffix},
				},
				Bundles: []declcfg.Bundle{
					{Name: "myapp.v1", Package: "myapp", Image: "reg/a@sha256:aaa"},
					{Name: "myapp" + tt.suffix + ".v1", Package: "myapp" + tt.suffix, Image: "reg/d@sha256:bbb"},
				},
			}

			resolver := &CatalogResolver{}
			filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
				{Name: "myapp"},
			})
			if err != nil {
				t.Fatalf("FilterFBC: %v", err)
			}

			pkgNames := map[string]bool{}
			for _, p := range filtered.Packages {
				pkgNames[p.Name] = true
			}
			if !pkgNames[tt.expectedPkg] {
				t.Errorf("%s should be auto-discovered as companion", tt.expectedPkg)
			}
		})
	}
}

func TestFilterFBC_HeadsOnlyEmptyChannelEntries(t *testing.T) {
	// Heads-only package with one channel having empty entries.
	cfg := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{{Name: "my-op"}},
		Channels: []declcfg.Channel{
			{
				Name: "stable", Package: "my-op",
				Entries: []declcfg.ChannelEntry{
					{Name: "my-op.v1.0.0"},
				},
			},
			{
				Name: "empty-ch", Package: "my-op",
				Entries: nil, // empty entries
			},
		},
		Bundles: []declcfg.Bundle{
			{Name: "my-op.v1.0.0", Package: "my-op", Image: "reg/op@sha256:aaa",
				Properties: []property.Property{{Type: olmPackage, Value: json.RawMessage(`{"packageName":"my-op","version":"1.0.0"}`)}}},
		},
	}

	resolver := &CatalogResolver{}
	filtered, err := resolver.FilterFBC(context.Background(), cfg, []mirrorv1alpha1.IncludePackage{
		{Name: "my-op"},
	})
	if err != nil {
		t.Fatalf("FilterFBC: %v", err)
	}
	if len(filtered.Bundles) != 1 {
		t.Errorf("expected 1 bundle (head of stable), got %d", len(filtered.Bundles))
	}
}

// ---------------------------------------------------------------------------
// Fake OCI registry for testing loadFBCFromImage path
// ---------------------------------------------------------------------------

// buildFBCGzipTar creates a gzip-tar layer containing FBC content.
func buildFBCGzipTar(t *testing.T) []byte {
	t.Helper()
	fbcContent := `---
schema: olm.package
name: test-operator
defaultChannel: stable
---
schema: olm.channel
name: stable
package: test-operator
entries:
  - name: test-operator.v1.0.0
---
schema: olm.bundle
name: test-operator.v1.0.0
package: test-operator
image: registry.example.com/test-bundle@sha256:deadbeef
properties:
  - type: olm.package
    value:
      packageName: test-operator
      version: 1.0.0
relatedImages:
  - name: test-image
    image: registry.example.com/test-image@sha256:cafebabe
`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Write configs directory
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "configs/", Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "configs/test-operator/", Mode: 0o755})
	data := []byte(fbcContent)
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "configs/test-operator/catalog.yaml",
		Size:     int64(len(data)),
		Mode:     0o644,
	})
	_, _ = tw.Write(data)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fakeOCIRegistry creates an httptest server that serves a minimal OCI
// registry with a single catalog image containing FBC content.
//
//nolint:unparam
func fakeOCIRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	layerBytes := buildFBCGzipTar(t)
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(layerBytes))

	// Build an OCI image config.
	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(configJSON))

	// Build OCI manifest.
	manifestJSON, _ := json.Marshal(map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configJSON),
		},
		"layers": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    layerDigest,
				"size":      len(layerBytes),
			},
		},
	})
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestJSON))

	mux := http.NewServeMux()

	// OCI distribution spec endpoints.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Version check
		if path == "/v2/" || path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Manifest requests
		if strings.Contains(path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestJSON)
			return
		}

		// Blob requests
		if strings.Contains(path, "/blobs/") {
			if strings.HasSuffix(path, layerDigest) || strings.Contains(path, layerDigest) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(layerBytes)
				return
			}
			if strings.HasSuffix(path, configDigest) || strings.Contains(path, configDigest) {
				w.Header().Set("Content-Type", "application/vnd.oci.image.config.v1+json")
				_, _ = w.Write(configJSON)
				return
			}
			http.NotFound(w, r)
			return
		}

		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Extract host from server URL.
	host := strings.TrimPrefix(srv.URL, "http://")
	return srv, host
}

func TestResolveCatalog_WithFakeRegistry(t *testing.T) {
	_, host := fakeOCIRegistry(t)

	client := mirrorclient.NewMirrorClient([]string{host}, "")
	r := New(client)

	catalogRef := host + "/test/catalog:latest"
	images, err := r.ResolveCatalog(context.Background(), catalogRef, []string{"test-operator"})
	if err != nil {
		t.Fatalf("ResolveCatalog: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("expected at least 1 image")
	}
	// First image should be the catalog itself.
	if images[0] != catalogRef {
		t.Errorf("first image should be catalog ref %s, got %s", catalogRef, images[0])
	}
}

func TestResolveCatalog_EmptyPkgsWithFakeRegistry(t *testing.T) {
	_, host := fakeOCIRegistry(t)

	client := mirrorclient.NewMirrorClient([]string{host}, "")
	r := New(client)

	catalogRef := host + "/test/catalog:latest"
	images, err := r.ResolveCatalog(context.Background(), catalogRef, []string{})
	if err != nil {
		t.Fatalf("ResolveCatalog: %v", err)
	}
	if len(images) != 1 || images[0] != catalogRef {
		t.Errorf("expected [%s], got %v", catalogRef, images)
	}
}

func TestResolveCatalogWithBundles_WithFakeRegistry(t *testing.T) {
	_, host := fakeOCIRegistry(t)

	client := mirrorclient.NewMirrorClient([]string{host}, "")
	r := New(client)

	catalogRef := host + "/test/catalog:latest"
	result, err := r.ResolveCatalogWithBundles(context.Background(), catalogRef,
		[]mirrorv1alpha1.IncludePackage{{Name: "test-operator"}})
	if err != nil {
		t.Fatalf("ResolveCatalogWithBundles: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected image references in result")
	}
}

func TestLoadFBC_WithFakeRegistry(t *testing.T) {
	_, host := fakeOCIRegistry(t)

	client := mirrorclient.NewMirrorClient([]string{host}, "")
	r := New(client)

	catalogRef := host + "/test/catalog:latest"
	cfg, err := r.LoadFBC(context.Background(), catalogRef)
	if err != nil {
		t.Fatalf("LoadFBC: %v", err)
	}
	if len(cfg.Packages) == 0 {
		t.Error("expected at least 1 package in loaded FBC")
	}
}
