package chartmirror

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// fakePushRegistry accepts blob uploads and manifest pushes for any
// repository, recording every manifest PUT so the test can verify the pushed
// Helm OCI artifact layout. Returns the server host and a channel receiving
// the pushed manifest bodies keyed by their repository path.
func fakePushRegistry(t *testing.T) (host string, manifests chan string) {
	t.Helper()
	manifests = make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v2/" || path == "/v2":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads/"):
			w.Header().Set("Location", path)
			w.Header().Set("Docker-Upload-UUID", "test-uuid")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.Contains(path, "/manifests/"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read manifest body: %v", err)
			}
			manifests <- string(body)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), manifests
}

func TestPushChart_ProducesHelmOCIArtifact(t *testing.T) {
	host, manifests := fakePushRegistry(t)
	archive := []byte("fake-chart-archive-tgz-bytes")

	p := NewPusher(mirrorclient.NewMirrorClient([]string{host}, ""))
	dest, err := p.PushChart(context.Background(), host, "bitnami", "nginx", "15.5.1", archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := host + "/charts/bitnami/nginx:15.5.1"
	if dest != want {
		t.Errorf("destination = %q, want %q", dest, want)
	}

	raw := <-manifests
	var m struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int    `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("parse pushed manifest: %v\nmanifest: %s", err, raw)
	}
	if m.Config.MediaType != "application/vnd.cncf.helm.config.v1+json" {
		t.Errorf("config media type = %q, want helm config media type", m.Config.MediaType)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(m.Layers))
	}
	if m.Layers[0].MediaType != ChartLayerMediaType {
		t.Errorf("layer media type = %q, want %q", m.Layers[0].MediaType, ChartLayerMediaType)
	}
	if want := digest.FromBytes(archive).String(); m.Layers[0].Digest != want {
		t.Errorf("layer digest = %q, want %q", m.Layers[0].Digest, want)
	}
	if m.Layers[0].Size != len(archive) {
		t.Errorf("layer size = %d, want %d", m.Layers[0].Size, len(archive))
	}
}

func TestPushChart_NoClient(t *testing.T) {
	p := NewPusher(nil)
	if _, err := p.PushChart(context.Background(), "reg.example.com", "repo", "chart", "1.0.0", []byte("x")); err == nil || !strings.Contains(err.Error(), "no registry client") {
		t.Errorf("expected a no-registry-client error, got %v", err)
	}
}

func TestPushChart_RequiresNameAndVersion(t *testing.T) {
	p := NewPusher(mirrorclient.NewMirrorClient(nil, ""))
	if _, err := p.PushChart(context.Background(), "reg.example.com", "repo", "", "1.0.0", []byte("x")); err == nil {
		t.Error("expected an error for an empty chart name")
	}
	if _, err := p.PushChart(context.Background(), "reg.example.com", "repo", "chart", "", []byte("x")); err == nil {
		t.Error("expected an error for an empty chart version")
	}
}
