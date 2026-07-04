package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// loadArchiveForTest loads a chart archive directly, bypassing the
// Resolver's HTTP download — used by tests that only exercise the
// rendering/image-extraction logic.
func loadArchiveForTest(t *testing.T, archive []byte) (*helmchart.Chart, error) {
	t.Helper()
	return loader.LoadArchive(bytes.NewReader(archive))
}

// buildTestChartArchive builds a minimal, valid Helm chart .tgz named
// "mychart" version "1.0.0", containing the given template files (path
// relative to templates/).
func buildTestChartArchive(t *testing.T, templates map[string]string) []byte {
	t.Helper()
	const name = "mychart"
	const version = "1.0.0"
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	files := map[string]string{
		name + "/Chart.yaml": "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
	}
	for path, content := range templates {
		files[name+"/templates/"+path] = content
	}

	for path, content := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path,
			Size:     int64(len(content)),
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header for %s: %v", path, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write content for %s: %v", path, err)
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

const testDeploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
        - name: myapp
          image: quay.io/example/myapp:1.2.3
      initContainers:
        - name: init
          image: quay.io/example/myapp-init:1.2.3
`

func TestImagesFromChart_DefaultPaths(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{
		"deployment.yaml": testDeploymentTemplate,
	})

	loaded, loadErr := loadArchiveForTest(t, archive)
	if loadErr != nil {
		t.Fatalf("unexpected error: %v", loadErr)
	}

	images, err := imagesFromChart(loaded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"quay.io/example/myapp-init:1.2.3", "quay.io/example/myapp:1.2.3"}
	if !equalStrings(images, want) {
		t.Errorf("expected %v, got %v", want, images)
	}
}

func TestImagesFromChart_CustomImagePath(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{
		"job.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  name: myjob
spec:
  extraImage: quay.io/example/extra:v9
`,
	})
	loaded, err := loadArchiveForTest(t, archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	images, err := imagesFromChart(loaded, "{.spec.extraImage}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"quay.io/example/extra:v9"}
	if !equalStrings(images, want) {
		t.Errorf("expected %v, got %v", want, images)
	}
}

func TestImagesFromChart_DeduplicatesAndSorts(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{
		"a.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: a
spec:
  template:
    spec:
      containers:
        - name: c
          image: quay.io/example/shared:v1
`,
		"b.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: b
spec:
  template:
    spec:
      containers:
        - name: c
          image: quay.io/example/shared:v1
`,
	})
	loaded, err := loadArchiveForTest(t, archive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	images, err := imagesFromChart(loaded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 || images[0] != "quay.io/example/shared:v1" {
		t.Errorf("expected deduplicated single image, got %v", images)
	}
}

func TestResolveChart_EndToEnd(t *testing.T) {
	archive := buildTestChartArchive(t, map[string]string{
		"deployment.yaml": testDeploymentTemplate,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		_, _ = w.Write([]byte(`apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.0.0
      urls:
        - mychart-1.0.0.tgz
`))
	})
	mux.HandleFunc("/mychart-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New()
	images, err := r.ResolveChart(context.Background(), srv.URL, mirrorv1alpha1.Chart{Name: "mychart", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"quay.io/example/myapp-init:1.2.3", "quay.io/example/myapp:1.2.3"}
	if !equalStrings(images, want) {
		t.Errorf("expected %v, got %v", want, images)
	}
}

func TestResolveChart_ChartNotFoundInIndex(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nentries: {}\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New()
	_, err := r.ResolveChart(context.Background(), srv.URL, mirrorv1alpha1.Chart{Name: "missing"})
	if err == nil {
		t.Fatal("expected error for chart not present in index")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveChart_IndexNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := New()
	_, err := r.ResolveChart(context.Background(), srv.URL, mirrorv1alpha1.Chart{Name: "mychart"})
	if err == nil {
		t.Fatal("expected error when index.yaml is not found")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
