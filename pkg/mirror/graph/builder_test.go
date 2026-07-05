package graph

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

// buildTestArchive produces a minimal gzip-tar archive with the given
// path -> content entries, matching the shape of the real Cincinnati
// graph-data download.
func buildTestArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestTargetImage(t *testing.T) {
	got := TargetImage("registry.example.com/mirror")
	want := "registry.example.com/mirror/openshift/graph-image:latest"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestBuildGraphDataLayer_RootsEntriesUnderGraphDataDir(t *testing.T) {
	archive := buildTestArchive(t, map[string]string{
		"channels/4.14.yaml":   "channel: stable-4.14\n",
		"blocked-edges/x.yaml": "reason: blocked\n",
	})

	data, diffID, err := buildGraphDataLayer(archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty gzip data")
	}
	if diffID.String() == "" {
		t.Fatal("expected non-empty diff ID")
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not valid gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	seen := map[string]string{}
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar read error: %v", nextErr)
		}
		content, _ := io.ReadAll(tr)
		seen[hdr.Name] = string(content)
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("expected uid/gid reset to 0 for %s, got uid=%d gid=%d", hdr.Name, hdr.Uid, hdr.Gid)
		}
	}

	wantChannel := "/var/lib/cincinnati-graph-data/channels/4.14.yaml"
	if content, ok := seen[wantChannel]; !ok {
		t.Errorf("expected entry %s, got entries: %v", wantChannel, seen)
	} else if content != "channel: stable-4.14\n" {
		t.Errorf("unexpected content for %s: %q", wantChannel, content)
	}

	wantBlocked := "/var/lib/cincinnati-graph-data/blocked-edges/x.yaml"
	if _, ok := seen[wantBlocked]; !ok {
		t.Errorf("expected entry %s, got entries: %v", wantBlocked, seen)
	}
}

func TestBuildGraphDataLayer_InvalidGzip(t *testing.T) {
	_, _, err := buildGraphDataLayer([]byte("not gzip data"))
	if err == nil {
		t.Fatal("expected error for invalid gzip input")
	}
}

func TestDownloadGraphData(t *testing.T) {
	original := graphDataURL
	defer func() { graphDataURL = original }()

	archive := buildTestArchive(t, map[string]string{"channels/4.14.yaml": "x"})

	t.Run("returns archive bytes on HTTP 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(archive)
		}))
		defer srv.Close()
		graphDataURL = srv.URL

		b := New(nil)
		got, err := b.downloadGraphData(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(got, archive) {
			t.Error("downloaded bytes do not match served archive")
		}
	})

	t.Run("returns error on non-200 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		graphDataURL = srv.URL

		b := New(nil)
		_, err := b.downloadGraphData(context.Background())
		if err == nil {
			t.Fatal("expected error for HTTP 503")
		}
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("returns error on empty body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		graphDataURL = srv.URL

		b := New(nil)
		_, err := b.downloadGraphData(context.Background())
		if err == nil {
			t.Fatal("expected error for empty body")
		}
		if !strings.Contains(err.Error(), "empty response body") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

func TestBuildAndPush_NilClient(t *testing.T) {
	b := New(nil)
	_, err := b.BuildAndPush(context.Background(), "registry.example.com/mirror")
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
	if !strings.Contains(err.Error(), "registry client is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestBuildAndPush_InvalidTargetRegistry(t *testing.T) {
	b := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := b.BuildAndPush(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for an empty target registry")
	}
}

// fakeUBIRegistry serves a minimal single-layer OCI image at <host>/ubi9/ubi:latest,
// standing in for the real registry.access.redhat.com/ubi9/ubi base image so
// DownloadToOCILayout can exercise its full pull path against a local server.
func fakeUBIRegistry(t *testing.T) (host string) {
	t.Helper()

	layerBytes := []byte("fake-ubi-layer-content")
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(layerBytes))
	diffID := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	configJSON, _ := json.Marshal(map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]interface{}{},
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
	})
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(configJSON))

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
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v2/" || path == "/v2":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestJSON)
		case strings.Contains(path, "/blobs/"):
			switch {
			case strings.Contains(path, layerDigest):
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(layerBytes)
			case strings.Contains(path, configDigest):
				w.Header().Set("Content-Type", "application/vnd.oci.image.config.v1+json")
				_, _ = w.Write(configJSON)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestBuildAndPush_CopiesBaseLayersThenFailsAgainstUnreachableDestination(t *testing.T) {
	origBase := graphBaseImage
	origDataURL := graphDataURL
	defer func() {
		graphBaseImage = origBase
		graphDataURL = origDataURL
	}()

	host := fakeUBIRegistry(t)
	graphBaseImage = host + "/ubi9/ubi:latest"

	archiveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildTestArchive(t, map[string]string{"channels/4.14.yaml": "x"}))
	}))
	defer archiveSrv.Close()
	graphDataURL = archiveSrv.URL

	// The base image host is reachable (insecure HTTP), but the destination
	// ("localhost:1") is not — nothing listens there, so the blob-copy loop
	// fails fast (connection refused) instead of hanging.
	client := mirrorclient.NewMirrorClient([]string{host}, "")
	b := New(client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := b.BuildAndPush(ctx, "localhost:1/mirror")
	if err == nil {
		t.Fatal("expected error copying to an unreachable destination registry")
	}
	if !strings.Contains(err.Error(), "copy base layer") {
		t.Errorf("expected failure while copying base layers (meaning download+config resolution succeeded), got: %v", err)
	}
}

func TestResolvePlatformManifest_NotAList(t *testing.T) {
	mc := mirrorclient.NewMirrorClient(nil, "")
	r, _ := ref.New("localhost:1/test:latest")
	m, err := manifest.New(manifest.WithOrig(v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: "application/vnd.oci.image.manifest.v1+json",
	}))
	if err != nil {
		t.Fatalf("failed to create test manifest: %v", err)
	}

	outRef, outM, err := resolvePlatformManifest(context.Background(), mc, r, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outRef.Tag != r.Tag {
		t.Errorf("expected ref unchanged, got tag=%q", outRef.Tag)
	}
	if outM != m {
		t.Error("expected same manifest returned")
	}
}

func TestResolvePlatformManifest_NoMatchingPlatform(t *testing.T) {
	mc := mirrorclient.NewMirrorClient(nil, "")
	r, _ := ref.New("localhost:1/test:latest")

	m, err := manifest.New(manifest.WithOrig(v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []descriptor.Descriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				Size:      2,
				Platform:  &platform.Platform{OS: "linux", Architecture: "arm64"},
			},
		},
	}))
	if err != nil {
		t.Fatalf("failed to create test manifest list: %v", err)
	}

	_, _, err = resolvePlatformManifest(context.Background(), mc, r, m)
	if err == nil {
		t.Fatal("expected error when no linux/amd64 manifest is present")
	}
	if !strings.Contains(err.Error(), "no linux/amd64 manifest in") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCopyBlobWithRetry_RespectsParentCancel(t *testing.T) {
	mc := mirrorclient.NewMirrorClient(nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := copyBlobWithRetry(ctx, mc, ref.Ref{}, ref.Ref{}, descriptor.Descriptor{}, 3, time.Minute)
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

func TestCopyBlobOnce_InvalidSource(t *testing.T) {
	mc := mirrorclient.NewMirrorClient(nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	srcRef, _ := ref.New("localhost:1/src:latest")
	dstRef, _ := ref.New("localhost:1/dst:latest")
	d := descriptor.Descriptor{Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	err := copyBlobOnce(ctx, mc, srcRef, dstRef, d)
	if err == nil {
		t.Error("expected error from copyBlobOnce with unreachable hosts")
	}
}
