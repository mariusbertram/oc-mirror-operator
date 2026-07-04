package graph

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
