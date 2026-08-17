package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	godigest "github.com/opencontainers/go-digest"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

// This file builds local OCI-layout ("ocidir://") fixtures directly through
// MirrorClient's Blob/Manifest calls, rather than an HTTP fake registry.
// regclient's ocidir scheme implements the full Blob/Manifest read+write
// surface BuildFilteredCatalogImage needs (BlobGet/BlobPut/BlobHead,
// ManifestGet/ManifestHead/ManifestPut), so a source AND a target catalog
// can be constructed and pushed to entirely on local disk — no wire-protocol
// reimplementation required, unlike pkg/mirror/graph's fakeUBIRegistry which
// genuinely needs to sit on the network path DownloadToOCILayout pulls from.

// pushOCIBlob writes data as a blob into the repository addressed by r and
// returns the descriptor (with the digest ocidir computed) for referencing
// it from a manifest.
func pushOCIBlob(t *testing.T, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, mt string, data []byte) descriptor.Descriptor {
	t.Helper()
	desc, err := client.BlobPut(ctx, r, descriptor.Descriptor{MediaType: mt, Size: int64(len(data))}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("BlobPut: %v", err)
	}
	return desc
}

// pushOCIManifest serialises orig (a v1.Manifest or v1.Index) and pushes it
// to r, returning the resulting manifest.Manifest (whose GetDescriptor()
// carries the digest other manifests can reference it by).
func pushOCIManifest(t *testing.T, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, orig any) manifest.Manifest {
	t.Helper()
	m, err := manifest.New(manifest.WithOrig(orig))
	if err != nil {
		t.Fatalf("manifest.New: %v", err)
	}
	if err := client.ManifestPut(ctx, r, m); err != nil {
		t.Fatalf("ManifestPut: %v", err)
	}
	return m
}

// buildFBCOnlyLayer returns a gzip-tar layer containing only configs/ content
// for pkgName — classifySourceLayers should find this skippable.
func buildFBCOnlyLayer(t *testing.T, pkgName string) []byte {
	t.Helper()
	yaml := "schema: olm.package\nname: " + pkgName + "\ndefaultChannel: stable\n---\n" +
		"schema: olm.channel\nname: stable\npackage: " + pkgName + "\nentries:\n  - name: " + pkgName + ".v1.0.0\n---\n" +
		"schema: olm.bundle\nname: " + pkgName + ".v1.0.0\npackage: " + pkgName + "\n" +
		"image: registry.example.com/" + pkgName + "-bundle@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000\n" +
		"properties:\n  - type: olm.package\n    value:\n      packageName: " + pkgName + "\n      version: 1.0.0\n"
	return makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
		{name: "configs/" + pkgName + "/", typeflag: tar.TypeDir},
		{name: "configs/" + pkgName + "/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(yaml)), body: []byte(yaml)},
	})
}

// pushSourceCatalog builds a two-manifest (index + linux/amd64 child) source
// catalog under an ocidir tagged "source" in a fresh temp dir, and returns
// its ocidir:// reference string. The child manifest has three layers
// exercising all three classifySourceLayers outcomes:
//   - layer 1: FBC-only -> skippable
//   - layer 2: non-FBC content -> kept, classifies cleanly
//   - layer 3: not actually gzip -> kept via the classification-failed path
func pushSourceCatalog(t *testing.T, pkgName string) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")

	sourceDir := t.TempDir()
	srcRef, err := ref.New("ocidir://" + sourceDir + ":source")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	layer1 := buildFBCOnlyLayer(t, pkgName)
	layer2 := makeGzipTar(t, []tarEntry{{name: "bin/opm", typeflag: tar.TypeReg, size: 6, body: []byte("binary")}})
	layer3 := []byte("this is not a valid gzip stream")

	l1Desc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1LayerGzip, layer1)
	l2Desc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1LayerGzip, layer2)
	l3Desc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1LayerGzip, layer3)

	diffIDs := []godigest.Digest{
		godigest.FromString("diff-id-1"),
		godigest.FromString("diff-id-2"),
		godigest.FromString("diff-id-3"),
	}
	configJSON, err := json.Marshal(map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]interface{}{},
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": diffIDs,
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfgDesc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1ImageConfig, configJSON)

	childM := pushOCIManifest(t, ctx, client, srcRef, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    []descriptor.Descriptor{l1Desc, l2Desc, l3Desc},
	})
	childDesc := childM.GetDescriptor()

	childRef := srcRef
	childRef.Digest = childDesc.Digest.String()
	childRef.Tag = ""
	if err := client.ManifestPut(ctx, childRef, childM); err != nil {
		t.Fatalf("ManifestPut child: %v", err)
	}

	pushOCIManifest(t, ctx, client, srcRef, v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: mediatype.OCI1ManifestList,
		Manifests: []descriptor.Descriptor{
			{
				MediaType: mediatype.OCI1Manifest,
				Digest:    childDesc.Digest,
				Size:      childDesc.Size,
				Platform:  &platform.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
	})

	return "ocidir://" + sourceDir + ":source"
}

// ---------------------------------------------------------------------------
// BuildFilteredCatalogImage
// ---------------------------------------------------------------------------

func TestBuildFilteredCatalogImage_FullSuccess(t *testing.T) {
	ctx := context.Background()
	sourceImage := pushSourceCatalog(t, "pkg-a")

	targetDir := t.TempDir()
	targetImage := "ocidir://" + targetDir + ":target"

	client := mirrorclient.NewMirrorClient(nil, "")
	resolver := New(client)

	digestStr, err := resolver.BuildFilteredCatalogImage(ctx, sourceImage, targetImage,
		[]mirrorv1alpha1.IncludePackage{{Name: "pkg-a"}})
	if err != nil {
		t.Fatalf("BuildFilteredCatalogImage: %v", err)
	}
	if digestStr == "" {
		t.Fatal("expected a non-empty manifest digest")
	}

	targetRef, err := ref.New(targetImage)
	if err != nil {
		t.Fatalf("ref.New(targetImage): %v", err)
	}
	pushedM, err := client.ManifestGet(ctx, targetRef)
	if err != nil {
		t.Fatalf("ManifestGet(target): %v", err)
	}
	layers, err := pushedM.GetLayers() //nolint:staticcheck
	if err != nil {
		t.Fatalf("GetLayers: %v", err)
	}
	// layer1 (FBC-only) is dropped; layer2 + layer3 are kept as-is, plus the
	// new filtered-FBC overlay layer this build produces.
	if len(layers) != 3 {
		t.Errorf("expected 3 layers in pushed catalog (2 kept + 1 FBC overlay), got %d", len(layers))
	}
	if pushedM.GetDescriptor().Digest.String() != digestStr {
		t.Errorf("returned digest %s does not match pushed manifest digest %s", digestStr, pushedM.GetDescriptor().Digest.String())
	}
}

func TestBuildFilteredCatalogImage_EarlyExitAlreadyUpToDate(t *testing.T) {
	ctx := context.Background()
	sourceImage := pushSourceCatalog(t, "pkg-b")

	targetDir := t.TempDir()
	targetImage := "ocidir://" + targetDir + ":target"

	client := mirrorclient.NewMirrorClient(nil, "")
	resolver := New(client)
	includes := []mirrorv1alpha1.IncludePackage{{Name: "pkg-b"}}

	first, err := resolver.BuildFilteredCatalogImage(ctx, sourceImage, targetImage, includes)
	if err != nil {
		t.Fatalf("first BuildFilteredCatalogImage: %v", err)
	}

	// Second call against the same source+target+filter is deterministic, so
	// the manifest digest it would produce already matches what's at
	// targetImage — the early-exit path (step 11) should trigger and skip
	// every upload.
	second, err := resolver.BuildFilteredCatalogImage(ctx, sourceImage, targetImage, includes)
	if err != nil {
		t.Fatalf("second BuildFilteredCatalogImage: %v", err)
	}
	if second != first {
		t.Errorf("expected idempotent digest, first=%s second=%s", first, second)
	}
}

func TestBuildFilteredCatalogImage_ReusesExistingBlobsUnderNewTag(t *testing.T) {
	ctx := context.Background()
	sourceImage := pushSourceCatalog(t, "pkg-c")

	// ocidir content-addresses blobs by digest within the directory, shared
	// across every tag in it. Pushing to a second tag in the SAME directory
	// after the first build means every kept layer, the FBC overlay layer,
	// and the image config all already exist there — while the manifest for
	// the new tag itself does not, so the early-exit check (step 11) still
	// misses and the build proceeds through steps 12-14's "already present,
	// skipping upload" branches instead of re-uploading anything.
	targetDir := t.TempDir()
	client := mirrorclient.NewMirrorClient(nil, "")
	resolver := New(client)
	includes := []mirrorv1alpha1.IncludePackage{{Name: "pkg-c"}}

	if _, err := resolver.BuildFilteredCatalogImage(ctx, sourceImage, "ocidir://"+targetDir+":target-a", includes); err != nil {
		t.Fatalf("first BuildFilteredCatalogImage: %v", err)
	}

	digestStr, err := resolver.BuildFilteredCatalogImage(ctx, sourceImage, "ocidir://"+targetDir+":target-b", includes)
	if err != nil {
		t.Fatalf("second BuildFilteredCatalogImage (new tag, shared blob store): %v", err)
	}
	if digestStr == "" {
		t.Fatal("expected a non-empty manifest digest")
	}

	targetBRef, err := ref.New("ocidir://" + targetDir + ":target-b")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}
	if _, err := client.ManifestGet(ctx, targetBRef); err != nil {
		t.Fatalf("expected target-b manifest to have been pushed: %v", err)
	}
}

func TestBuildFilteredCatalogImage_LayerConfigMismatch(t *testing.T) {
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	sourceDir := t.TempDir()
	srcRef, _ := ref.New("ocidir://" + sourceDir + ":source")

	layer := makeGzipTar(t, []tarEntry{{name: "bin/opm", typeflag: tar.TypeReg, size: 6, body: []byte("binary")}})
	layerDesc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1LayerGzip, layer)
	// Config declares two diff_ids but the manifest below lists only one layer.
	configJSON, _ := json.Marshal(map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]interface{}{},
		"rootfs": map[string]interface{}{
			"type": "layers",
			"diff_ids": []godigest.Digest{
				godigest.FromString("diff-id-1"),
				godigest.FromString("diff-id-2"),
			},
		},
	})
	cfgDesc := pushOCIBlob(t, ctx, client, srcRef, mediatype.OCI1ImageConfig, configJSON)
	pushOCIManifest(t, ctx, client, srcRef, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    []descriptor.Descriptor{layerDesc},
	})

	resolver := New(client)
	targetDir := t.TempDir()
	_, err := resolver.BuildFilteredCatalogImage(ctx, "ocidir://"+sourceDir+":source", "ocidir://"+targetDir+":target", nil)
	if err == nil {
		t.Fatal("expected error for mismatched layer/diff_id counts")
	}
	if !strings.Contains(err.Error(), "manifest/config mismatch") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// loadFBCFromImage / LoadFBC — manifest-list source
// ---------------------------------------------------------------------------

func TestLoadFBC_ManifestListSource(t *testing.T) {
	sourceImage := pushSourceCatalog(t, "pkg-list")

	client := mirrorclient.NewMirrorClient(nil, "")
	resolver := New(client)

	cfg, err := resolver.LoadFBC(context.Background(), sourceImage)
	if err != nil {
		t.Fatalf("LoadFBC: %v", err)
	}
	if len(cfg.Packages) != 1 || cfg.Packages[0].Name != "pkg-list" {
		t.Errorf("expected package pkg-list, got %+v", cfg.Packages)
	}
}

func TestLoadFBC_InvalidCatalogImageRef(t *testing.T) {
	client := mirrorclient.NewMirrorClient(nil, "")
	resolver := New(client)

	_, err := resolver.LoadFBC(context.Background(), "\x00not a valid ref")
	if err == nil {
		t.Fatal("expected error for an unparseable catalog image reference")
	}
	if !strings.Contains(err.Error(), "failed to parse image reference") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestLoadFBC_ManifestListNoMatchingPlatform(t *testing.T) {
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	dir := t.TempDir()
	r, _ := ref.New("ocidir://" + dir + ":source")

	pushOCIManifest(t, ctx, client, r, v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: mediatype.OCI1ManifestList,
		Manifests: []descriptor.Descriptor{
			{
				MediaType: mediatype.OCI1Manifest,
				Digest:    godigest.FromString("arm64-child"),
				Size:      2,
				Platform:  &platform.Platform{OS: "linux", Architecture: "arm64"},
			},
		},
	})

	resolver := New(client)
	_, err := resolver.LoadFBC(ctx, "ocidir://"+dir+":source")
	if err == nil {
		t.Fatal("expected error when the source has no linux/amd64 manifest")
	}
	if !strings.Contains(err.Error(), "no amd64 manifest in") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestLoadFBC_ManifestListPlatformFetchFails(t *testing.T) {
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	dir := t.TempDir()
	r, _ := ref.New("ocidir://" + dir + ":source")

	pushOCIManifest(t, ctx, client, r, v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: mediatype.OCI1ManifestList,
		Manifests: []descriptor.Descriptor{
			{
				MediaType: mediatype.OCI1Manifest,
				Digest:    godigest.FromString("missing-amd64-child"),
				Size:      2,
				Platform:  &platform.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
	})

	resolver := New(client)
	_, err := resolver.LoadFBC(ctx, "ocidir://"+dir+":source")
	if err == nil {
		t.Fatal("expected error when the platform manifest can't be fetched")
	}
	if !strings.Contains(err.Error(), "failed to get platform manifest") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestLoadFBC_NoFBCFilesNoBlobErrors(t *testing.T) {
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	dir := t.TempDir()
	r, _ := ref.New("ocidir://" + dir + ":source")

	layer := makeGzipTar(t, []tarEntry{{name: "bin/opm", typeflag: tar.TypeReg, size: 6, body: []byte("binary")}})
	layerDesc := pushOCIBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, layer)
	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfgDesc := pushOCIBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, configJSON)
	pushOCIManifest(t, ctx, client, r, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    []descriptor.Descriptor{layerDesc},
	})

	resolver := New(client)
	_, err := resolver.LoadFBC(ctx, "ocidir://"+dir+":source")
	if err == nil {
		t.Fatal("expected error when no layer contains configs/ content")
	}
	if !strings.Contains(err.Error(), "no FBC config files found under") || strings.Contains(err.Error(), "failed to read") {
		t.Errorf("expected the no-blob-errors error variant, got: %v", err)
	}
}

func TestLoadFBC_NoFBCFilesWithBlobErrors(t *testing.T) {
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	dir := t.TempDir()
	r, _ := ref.New("ocidir://" + dir + ":source")

	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfgDesc := pushOCIBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, configJSON)
	// This layer descriptor references a digest that was never actually
	// pushed as a blob, forcing BlobGet to fail for it inside the layer loop.
	missingLayer := descriptor.Descriptor{
		MediaType: mediatype.OCI1LayerGzip,
		Digest:    godigest.FromString("never-pushed-layer"),
		Size:      10,
	}
	pushOCIManifest(t, ctx, client, r, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    []descriptor.Descriptor{missingLayer},
	})

	resolver := New(client)
	_, err := resolver.LoadFBC(ctx, "ocidir://"+dir+":source")
	if err == nil {
		t.Fatal("expected error when the only layer is unreadable")
	}
	if !strings.Contains(err.Error(), "failed to read 1/1 layers") {
		t.Errorf("expected the blob-errors error variant, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// resolveManifestList — list-path error branches
// ---------------------------------------------------------------------------

func TestResolveManifestList_ErrorBranches(t *testing.T) {
	tests := []struct {
		name       string
		arch       string
		digest     string
		wantErrSub string
	}{
		{
			name:       "NoMatchingPlatform",
			arch:       "arm64",
			digest:     "arm64-child",
			wantErrSub: "no linux/amd64 manifest in",
		},
		{
			// The index references a well-formed linux/amd64 child
			// descriptor, but nothing was ever pushed at that digest — the
			// follow-up ManifestGet by digest must fail.
			name:       "PlatformManifestFetchFails",
			arch:       "amd64",
			digest:     "missing-child",
			wantErrSub: "failed to get platform manifest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := manifest.New(manifest.WithOrig(v1.Index{
				Versioned: v1.IndexSchemaVersion,
				MediaType: mediatype.OCI1ManifestList,
				Manifests: []descriptor.Descriptor{
					{
						MediaType: mediatype.OCI1Manifest,
						Digest:    godigest.FromString(tt.digest),
						Size:      2,
						Platform:  &platform.Platform{OS: "linux", Architecture: tt.arch},
					},
				},
			}))
			if err != nil {
				t.Fatalf("manifest.New: %v", err)
			}

			client := mirrorclient.NewMirrorClient(nil, "")
			r, _ := ref.New("ocidir://" + t.TempDir() + ":tag")

			_, _, err = resolveManifestList(context.Background(), client, r, m, "test-image")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("unexpected error message: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// highestVersionBundle / parseBundleSemver
// ---------------------------------------------------------------------------

func TestParseBundleSemver_BundleNotFound(t *testing.T) {
	_, ok := parseBundleSemver(map[string]declcfg.Bundle{}, "missing")
	if ok {
		t.Error("expected ok=false for a bundle name not present in the map")
	}
}

func TestParseBundleSemver_EmptyVersion(t *testing.T) {
	bundles := map[string]declcfg.Bundle{
		"op.v1": {Name: "op.v1", Package: "op"},
	}
	_, ok := parseBundleSemver(bundles, "op.v1")
	if ok {
		t.Error("expected ok=false when olm.package has no version")
	}
}

func TestParseBundleSemver_UnparseableVersion(t *testing.T) {
	bundles := map[string]declcfg.Bundle{
		"op.v1": {
			Name: "op.v1", Package: "op",
			Properties: []property.Property{
				{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"not-a-version!!"}`)},
			},
		},
	}
	_, ok := parseBundleSemver(bundles, "op.v1")
	if ok {
		t.Error("expected ok=false for an unparseable semver string")
	}
}

func TestHighestVersionBundle_SecondCandidateResolvesFirstUnknown(t *testing.T) {
	// candidates[0] has no parseable version (bestOK=false); candidates[1]
	// does — this must take over as best via the "ok && !bestOK" branch.
	bundles := map[string]declcfg.Bundle{
		"op.unknown": {Name: "op.unknown", Package: "op"},
		"op.v1": {
			Name: "op.v1", Package: "op",
			Properties: []property.Property{
				{Type: olmPackage, Value: json.RawMessage(`{"packageName":"op","version":"1.0.0"}`)},
			},
		},
	}
	got := highestVersionBundle([]string{"op.unknown", "op.v1"}, bundles)
	if got != "op.v1" {
		t.Errorf("expected op.v1 to win over an unversioned candidate, got %q", got)
	}
}

func TestHighestVersionBundle_BothUnknownTieBreakByName(t *testing.T) {
	// Neither candidate has a parseable version — falls back to name
	// comparison via the "!ok && !bestOK && c < best" branch.
	bundles := map[string]declcfg.Bundle{
		"op.b": {Name: "op.b", Package: "op"},
		"op.a": {Name: "op.a", Package: "op"},
	}
	got := highestVersionBundle([]string{"op.b", "op.a"}, bundles)
	if got != "op.a" {
		t.Errorf("expected lexicographically-first name op.a as deterministic fallback, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// repairChannelGraph — orphaned Replaces chain
// ---------------------------------------------------------------------------

func TestRepairChannelGraph_ReplacesPointsOutsideOriginalEntries(t *testing.T) {
	// entry "op.v2" replaces "op.v1", which is dropped; walking further back
	// from "op.v1" would normally continue via its own Replaces, but "op.v1"
	// itself is not present in the original entry set at all (e.g. a
	// malformed/partial channel), so the ancestor walk must stop cleanly
	// with entryByName lookup failing rather than looping or panicking.
	original := []declcfg.ChannelEntry{
		{Name: "op.v2", Replaces: "op.v1"},
	}
	kept := map[string]bool{"op.v2": true}
	result := repairChannelGraph(original, kept)
	if len(result) != 1 {
		t.Fatalf("expected 1 kept entry, got %d", len(result))
	}
	if result[0].Replaces != "" {
		t.Errorf("expected Replaces cleared when ancestor is entirely absent, got %q", result[0].Replaces)
	}
}

// ---------------------------------------------------------------------------
// blobCopyWithRetry — attempts exhausted
// ---------------------------------------------------------------------------

func TestBlobCopyWithRetry_ExhaustsAttemptsAndReturnsLastErr(t *testing.T) {
	mc := mirrorclient.NewMirrorClient([]string{"localhost:1"}, "")
	srcRef, _ := ref.New("localhost:1/src:latest")
	dstRef, _ := ref.New("localhost:1/dst:latest")
	d := descriptor.Descriptor{Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}

	// A live (non-cancelled) context with a short per-attempt timeout: every
	// attempt fails against the unreachable host, exercising the inter-attempt
	// backoff (attempt > 1) before returning the last real error rather than
	// ctx.Err().
	err := blobCopyWithRetry(context.Background(), mc, srcRef, dstRef, d, 2, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected error after exhausting all attempts")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("expected the last attempt's error, not the parent context's: %v", err)
	}
}
