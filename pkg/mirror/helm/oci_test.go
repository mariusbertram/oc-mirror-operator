package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// fakeOCIChartRegistry serves a Helm chart as an OCI artifact at
// <host>/charts/mychart:1.0.0 (plain HTTP) and, like Bitnami's repository,
// an index.yaml at /repo/index.yaml whose download URL points at that
// artifact via oci://. Returns the server's host.
func fakeOCIChartRegistry(t *testing.T, archive []byte, layerMediaType string) string {
	t.Helper()
	sum := sha256.Sum256(archive)
	layerDigest := "sha256:" + hex.EncodeToString(sum[:])
	const config = "{}"
	cfgSum := sha256.Sum256([]byte(config))
	cfgDigest := "sha256:" + hex.EncodeToString(cfgSum[:])
	manifestJSON := fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": %q, "size": %d},
  "layers": [{"mediaType": %q, "digest": %q, "size": %d}]
}`, cfgDigest, len(config), layerMediaType, layerDigest, len(archive))
	manSum := sha256.Sum256([]byte(manifestJSON))
	manDigest := "sha256:" + hex.EncodeToString(manSum[:])

	var host string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/charts/mychart/manifests/1.0.0", "/v2/charts/mychart/manifests/" + manDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manDigest)
			w.Header().Set("Content-Length", fmt.Sprint(len(manifestJSON)))
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(manifestJSON))
			}
		case "/v2/charts/mychart/blobs/" + layerDigest:
			w.Header().Set("Content-Length", fmt.Sprint(len(archive)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(archive)
			}
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/repo/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.0.0
      urls:
        - oci://%s/charts/mychart:1.0.0
`, host)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "http://")
	return host
}

// A repository index pointing at oci:// chart archives (Bitnami since 2024)
// used to fail with `unsupported protocol scheme "oci"` (#187).
func TestResolveChart_IndexWithOCIChartURL(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{"deployment.yaml": testDeploymentTemplate})
	host := fakeOCIChartRegistry(t, archive, "application/vnd.cncf.helm.chart.content.v1.tar+gzip")
	r := NewWithOCI(mirrorclient.NewMirrorClient([]string{host}, ""))

	images, err := r.ResolveChart(context.Background(), "http://"+host+"/repo", mirrorv1alpha1.Chart{Name: "mychart", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"quay.io/example/myapp-init:1.2.3", "quay.io/example/myapp:1.2.3"}
	if !equalStrings(images, want) {
		t.Errorf("expected %v, got %v", want, images)
	}
}

func TestResolveChart_OCIRepository(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{"deployment.yaml": testDeploymentTemplate})
	// Legacy layer media type pushed by Helm 3.0–3.7.
	host := fakeOCIChartRegistry(t, archive, "application/tar+gzip")
	r := NewWithOCI(mirrorclient.NewMirrorClient([]string{host}, ""))
	repo := "oci://" + host + "/charts"

	images, err := r.ResolveChart(context.Background(), repo, mirrorv1alpha1.Chart{Name: "mychart", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 2 {
		t.Errorf("expected 2 images, got %v", images)
	}

	_, err = r.ResolveChart(context.Background(), repo, mirrorv1alpha1.Chart{Name: "mychart"})
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Errorf("expected a version-required error for an OCI repository without version, got %v", err)
	}
}

func TestResolveChart_OCIChartWithoutRegistryClient(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{"deployment.yaml": testDeploymentTemplate})
	host := fakeOCIChartRegistry(t, archive, "application/vnd.cncf.helm.chart.content.v1.tar+gzip")

	_, err := New().ResolveChart(context.Background(), "http://"+host+"/repo", mirrorv1alpha1.Chart{Name: "mychart", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "no registry client") {
		t.Errorf("expected a clear no-registry-client error, got %v", err)
	}
}

func TestResolveChart_OCIArtifactWithoutChartLayer(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{"deployment.yaml": testDeploymentTemplate})
	host := fakeOCIChartRegistry(t, archive, "application/vnd.oci.image.layer.v1.tar+gzip")
	r := NewWithOCI(mirrorclient.NewMirrorClient([]string{host}, ""))

	_, err := r.ResolveChart(context.Background(), "oci://"+host+"/charts", mirrorv1alpha1.Chart{Name: "mychart", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "no Helm chart layer") {
		t.Errorf("expected a no-chart-layer error, got %v", err)
	}
}
