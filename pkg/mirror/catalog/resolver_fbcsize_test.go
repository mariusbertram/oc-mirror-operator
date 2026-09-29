package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

// Regression tests for issue #181: FBC files larger than the old 64 MiB
// io.LimitReader cap were stored truncated without an error, so declcfg later
// failed the whole catalog with a bare "unexpected EOF".

// oldFBCCap is the per-file cap the extraction code used before the fix.
const oldFBCCap = 64 << 20

// setMaxFBCFileSize lowers the per-file limit for one test.
func setMaxFBCFileSize(t *testing.T, limit int64) {
	t.Helper()
	orig := maxFBCFileSize
	maxFBCFileSize = limit
	t.Cleanup(func() { maxFBCFileSize = orig })
}

// bigPackageFBC returns a valid FBC JSON document for pkgName whose olm.package
// description pads it to more than minSize bytes. Cut anywhere, it is invalid JSON.
func bigPackageFBC(t *testing.T, pkgName string, minSize int) []byte {
	t.Helper()
	doc, err := json.Marshal(map[string]string{
		"schema":         "olm.package",
		"name":           pkgName,
		"defaultChannel": "stable",
		"description":    strings.Repeat("x", minSize),
	})
	if err != nil {
		t.Fatalf("marshal FBC: %v", err)
	}
	return doc
}

// pushSingleManifestImage writes a single-manifest image with the given gzip
// layers to a fresh ocidir and returns its reference string.
func pushSingleManifestImage(t *testing.T, layers ...[]byte) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")
	dir := t.TempDir()
	r, err := ref.New("ocidir://" + dir + ":source")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	descs := make([]descriptor.Descriptor, 0, len(layers))
	diffIDs := make([]godigest.Digest, 0, len(layers))
	for i, l := range layers {
		descs = append(descs, pushOCIBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, l))
		diffIDs = append(diffIDs, godigest.FromString("diff-id-"+string(rune('a'+i))))
	}
	configJSON, err := json.Marshal(map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]interface{}{},
		"rootfs":       map[string]interface{}{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfgDesc := pushOCIBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, configJSON)
	pushOCIManifest(t, ctx, client, r, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    descs,
	})
	return "ocidir://" + dir + ":source"
}

func TestExtractFBCLayer_FileOver64MiBExtractedInFull(t *testing.T) {
	body := bytes.Repeat([]byte("x"), oldFBCCap+6<<20)
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/big/catalog.json", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	fsMap := make(fstest.MapFS)
	count, err := extractFBCLayer(bytes.NewReader(data), fsMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 file, got %d", count)
	}
	if got := len(fsMap["configs/big/catalog.json"].Data); got != len(body) {
		t.Fatalf("file stored truncated: %d of %d bytes", got, len(body))
	}
}

func TestClassifyAndExtractFBC_FileOver64MiBExtractedInFull(t *testing.T) {
	body := bytes.Repeat([]byte("x"), oldFBCCap+6<<20)
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/big/catalog.json", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	fsMap := make(fstest.MapFS)
	skip, sz, _, err := classifyAndExtractFBC(bytes.NewReader(data), fsMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skip || sz != int64(len(body)) {
		t.Errorf("expected a skippable layer of %d bytes, got skip=%v size=%d", len(body), skip, sz)
	}
	if got := len(fsMap["configs/big/catalog.json"].Data); got != len(body) {
		t.Fatalf("file stored truncated: %d of %d bytes", got, len(body))
	}
}

func TestExtractFBC_FileSizeLimit(t *testing.T) {
	setMaxFBCFileSize(t, 16)
	atLimit := bytes.Repeat([]byte("a"), 16)
	overLimit := bytes.Repeat([]byte("b"), 17)
	entries := []tarEntry{
		{name: "configs/small/catalog.json", typeflag: tar.TypeReg, size: int64(len(atLimit)), body: atLimit},
		{name: "configs/huge/catalog.json", typeflag: tar.TypeReg, size: int64(len(overLimit)), body: overLimit},
	}

	checkErr := func(t *testing.T, err error, fsMap fstest.MapFS) {
		t.Helper()
		if !errors.Is(err, errIncompleteFBC) {
			t.Fatalf("expected errIncompleteFBC, got %v", err)
		}
		for _, want := range []string{"configs/huge/catalog.json", "17 bytes", "16 byte limit"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
		if _, ok := fsMap["configs/huge/catalog.json"]; ok {
			t.Error("oversized file must not be stored")
		}
		if got := string(fsMap["configs/small/catalog.json"].Data); got != string(atLimit) {
			t.Errorf("file at the limit should be stored in full, got %q", got)
		}
	}

	t.Run("extractFBCLayer", func(t *testing.T) {
		fsMap := make(fstest.MapFS)
		count, err := extractFBCLayer(bytes.NewReader(makeGzipTar(t, entries)), fsMap)
		checkErr(t, err, fsMap)
		if count != 1 {
			t.Errorf("expected 1 file before the error, got %d", count)
		}
	})
	t.Run("classifyAndExtractFBC", func(t *testing.T) {
		fsMap := make(fstest.MapFS)
		_, _, _, err := classifyAndExtractFBC(bytes.NewReader(makeGzipTar(t, entries)), fsMap)
		checkErr(t, err, fsMap)
	})
}

// TestLoadFBC_FileOver64MiB is the end-to-end reproduction of issue #181: a
// catalog whose layer holds one FBC file over 64 MiB must load, and a small
// package in the same catalog must be visible.
func TestLoadFBC_FileOver64MiB(t *testing.T) {
	big := bigPackageFBC(t, "big-op", oldFBCCap+2<<20)
	small := []byte(`{"schema":"olm.package","name":"small-op","defaultChannel":"stable"}`)
	layer := makeGzipTar(t, []tarEntry{
		{name: "configs/big-op/catalog.json", typeflag: tar.TypeReg, size: int64(len(big)), body: big},
		{name: "configs/small-op/catalog.json", typeflag: tar.TypeReg, size: int64(len(small)), body: small},
	})
	image := pushSingleManifestImage(t, layer)

	cfg, err := New(mirrorclient.NewMirrorClient(nil, "")).LoadFBC(context.Background(), image)
	if err != nil {
		t.Fatalf("LoadFBC: %v", err)
	}
	names := map[string]bool{}
	for _, p := range cfg.Packages {
		names[p.Name] = true
	}
	if !names["big-op"] || !names["small-op"] {
		t.Errorf("expected packages big-op and small-op, got %v", names)
	}
}

func TestLoadFBC_OversizedFileNamesFileAndLayer(t *testing.T) {
	setMaxFBCFileSize(t, 32)
	body := bytes.Repeat([]byte("z"), 33)
	layer := makeGzipTar(t, []tarEntry{
		{name: "configs/huge-op/catalog.json", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	image := pushSingleManifestImage(t, layer)

	_, err := New(mirrorclient.NewMirrorClient(nil, "")).LoadFBC(context.Background(), image)
	if !errors.Is(err, errIncompleteFBC) {
		t.Fatalf("expected errIncompleteFBC, got %v", err)
	}
	for _, want := range []string{godigest.FromBytes(layer).String(), "configs/huge-op/catalog.json", "32 byte limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadFBC_TruncatedLayerFails(t *testing.T) {
	good := buildFBCOnlyLayer(t, "pkg-a")
	// Cut the gzip stream in half: a partial catalog must never load.
	image := pushSingleManifestImage(t, good[:len(good)/2])

	_, err := New(mirrorclient.NewMirrorClient(nil, "")).LoadFBC(context.Background(), image)
	if !errors.Is(err, errIncompleteFBC) {
		t.Fatalf("expected errIncompleteFBC for a truncated layer, got %v", err)
	}
}

func TestBuildFilteredCatalogImage_OversizedFBCFileFails(t *testing.T) {
	setMaxFBCFileSize(t, 32)
	body := bytes.Repeat([]byte("z"), 33)
	fbcLayer := makeGzipTar(t, []tarEntry{
		{name: "configs/huge-op/catalog.json", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	binLayer := makeGzipTar(t, []tarEntry{{name: "bin/opm", typeflag: tar.TypeReg, size: 6, body: []byte("binary")}})
	image := pushSingleManifestImage(t, binLayer, fbcLayer)

	targetImage := "ocidir://" + t.TempDir() + ":target"
	_, err := New(mirrorclient.NewMirrorClient(nil, "")).BuildFilteredCatalogImage(
		context.Background(), image, targetImage, nil)
	if !errors.Is(err, errIncompleteFBC) {
		t.Fatalf("expected errIncompleteFBC, got %v", err)
	}
	for _, want := range []string{"failed to extract FBC", godigest.FromBytes(fbcLayer).String(), "configs/huge-op/catalog.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The fs.Sub error branch in parseExtractedFBC is unreachable: configsPath is
// a constant, valid path, so fs.Sub on a MapFS cannot fail.
func TestParseExtractedFBC_Errors(t *testing.T) {
	tests := []struct {
		name    string
		fs      fstest.MapFS
		wantErr string
	}{
		{"no config files", fstest.MapFS{}, "no FBC config files found under configs/ in img"},
		{"truncated JSON", fstest.MapFS{
			"configs/op/catalog.json": &fstest.MapFile{Data: []byte(`{"schema":"olm.package","name":"op","description":"cut`)},
		}, "failed to parse FBC from img"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseExtractedFBC(context.Background(), tt.fs, "img")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}
