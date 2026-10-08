package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/chartmirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// chartRepoAndRegistry serves a minimal chart repository (index.yaml +
// archive) and accepts pushes for any repository, so MirrorChart can run its
// full download → push path against one local server.
func chartRepoAndRegistry(t *testing.T, archive []byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  mychart:\n    - name: mychart\n      version: 1.0.0\n      urls:\n        - mychart-1.0.0.tgz\n"))
	})
	mux.HandleFunc("/mychart-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v2/" || path == "/v2":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads/"):
			w.Header().Set("Location", path)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// buildChartArchive builds a minimal valid Helm chart .tgz.
func buildChartArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	files := map[string]string{
		"mychart/Chart.yaml": "apiVersion: v2\nname: mychart\nversion: 1.0.0\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMirrorChart_FullPath(t *testing.T) {
	host := chartRepoAndRegistry(t, buildChartArchive(t))
	c := mirrorclient.NewMirrorClient([]string{host}, "", host)
	src := mirror.HelmChartSource("http://"+host, "mychart", "1.0.0")
	dest := chartmirror.ChartDestination(host, "test-repo", "mychart", "1.0.0")
	pushed, err := MirrorChart(context.Background(), c, src, dest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushed != dest {
		t.Errorf("pushed = %q, want %q", pushed, dest)
	}
}

func TestMirrorChart_InvalidSource(t *testing.T) {
	c := mirrorclient.NewMirrorClient(nil, "")
	if _, err := MirrorChart(context.Background(), c, "https://not-a-chart-source", "reg/charts/r/c:1.0.0"); err == nil {
		t.Fatal("expected an error for a non-helm:// source")
	}
}

func TestMirrorChart_InvalidDestination(t *testing.T) {
	c := mirrorclient.NewMirrorClient(nil, "")
	src := mirror.HelmChartSource("https://charts.example.com", "mychart", "1.0.0")
	if _, err := MirrorChart(context.Background(), c, src, "reg/not-a-chart-dest"); err == nil {
		t.Fatal("expected an error for an invalid chart destination")
	}
}

func TestMirrorChart_UnreachableRepository(t *testing.T) {
	c := mirrorclient.NewMirrorClient(nil, "")
	src := mirror.HelmChartSource("http://localhost:1/charts", "mychart", "1.0.0")
	if _, err := MirrorChart(context.Background(), c, src, "localhost:1/charts/repo/mychart:1.0.0"); err == nil {
		t.Fatal("expected a download error for an unreachable repository")
	}
}
