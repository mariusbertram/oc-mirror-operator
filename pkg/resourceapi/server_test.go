package resourceapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testMTName = "test-mt"

func setupTestServer(t *testing.T) *mux.Router {
	t.Helper()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	mtName := testMTName
	cmName := fmt.Sprintf("oc-mirror-%s-resources", mtName)
	packagesCmName := "oc-mirror-test-slug-packages"

	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mtName,
			Namespace: ns,
		},
		Spec: mirrorv1alpha1.MirrorTargetSpec{
			Registry:  "registry.example.com/mirror",
			ImageSets: []string{"is-one", "is-two"},
		},
		Status: mirrorv1alpha1.MirrorTargetStatus{
			TotalImages:    100,
			MirroredImages: 80,
			PendingImages:  15,
			FailedImages:   5,
			ImageSetStatuses: []mirrorv1alpha1.ImageSetStatusSummary{
				{Name: "is-one", Found: true, Total: 60, Mirrored: 50, Pending: 8, Failed: 2},
				{Name: "is-two", Found: true, Total: 40, Mirrored: 30, Pending: 7, Failed: 3},
			},
		},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ns,
		},
		Data: map[string]string{
			"idms.yaml":               "kind: ImageDigestMirrorSet\nmetadata:\n  name: test",
			"itms.yaml":               "kind: ImageTagMirrorSet\nmetadata:\n  name: test",
			"catalogsource-test.yaml": "kind: CatalogSource\nmetadata:\n  name: test",
		},
	}

	packagesCm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      packagesCmName,
			Namespace: ns,
		},
		Data: map[string]string{
			"packages.json": `{"catalog":"test","packages":[]}`,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt, cm, packagesCm).Build()
	s := NewServer(c, ns)

	r := mux.NewRouter()
	s.RegisterRoutes(r)

	return r
}

func TestResourceAPIEndpoints(t *testing.T) {
	router := setupTestServer(t)
	mtName := testMTName

	tests := []struct {
		name           string
		url            string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "Get IDMS",
			url:            fmt.Sprintf("/api/v1/targets/%s/resources/idms", mtName),
			expectedStatus: http.StatusOK,
			expectedBody:   "kind: ImageDigestMirrorSet",
		},
		{
			name:           "Get ITMS",
			url:            fmt.Sprintf("/api/v1/targets/%s/resources/itms", mtName),
			expectedStatus: http.StatusOK,
			expectedBody:   "kind: ImageTagMirrorSet",
		},
		{
			name:           "Get CatalogSource",
			url:            fmt.Sprintf("/api/v1/targets/%s/resources/catalogs/test/catalogsource", mtName),
			expectedStatus: http.StatusOK,
			expectedBody:   "kind: CatalogSource",
		},
		{
			name:           "Get Catalog Packages",
			url:            "/api/v1/targets/any/resources/catalogs/test-slug/packages",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"catalog":"test","packages":[]}`,
		},
		{
			name:           "Resource not found",
			url:            fmt.Sprintf("/api/v1/targets/%s/resources/catalogs/nonexistent/catalogsource", mtName),
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Legacy redirect",
			url:            "/resources/test-is/idms.yaml",
			expectedStatus: http.StatusGone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d (body: %s)", tt.expectedStatus, rr.Code, rr.Body.String())
			}
			if tt.expectedBody != "" && len(rr.Body.String()) >= len(tt.expectedBody) &&
				rr.Body.String()[:len(tt.expectedBody)] != tt.expectedBody {
				t.Errorf("expected body to start with %q, got %q", tt.expectedBody, rr.Body.String())
			}
		})
	}
}

func TestTargetsList(t *testing.T) {
	router := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/targets", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var targets []TargetSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &targets); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}

	tgt := targets[0]
	if tgt.Name != "test-mt" {
		t.Errorf("expected name test-mt, got %s", tgt.Name)
	}
	if tgt.Registry != "registry.example.com/mirror" {
		t.Errorf("expected registry registry.example.com/mirror, got %s", tgt.Registry)
	}
	if tgt.TotalImages != 100 {
		t.Errorf("expected totalImages=100, got %d", tgt.TotalImages)
	}
	if tgt.MirroredImages != 80 {
		t.Errorf("expected mirroredImages=80, got %d", tgt.MirroredImages)
	}
	if tgt.PendingImages != 15 {
		t.Errorf("expected pendingImages=15, got %d", tgt.PendingImages)
	}
	if tgt.FailedImages != 5 {
		t.Errorf("expected failedImages=5, got %d", tgt.FailedImages)
	}
}

func TestTargetDetail(t *testing.T) {
	router := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/targets/test-mt", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var detail TargetDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if detail.Name != "test-mt" {
		t.Errorf("expected name test-mt, got %s", detail.Name)
	}
	if detail.TotalImages != 100 {
		t.Errorf("expected totalImages=100, got %d", detail.TotalImages)
	}

	if len(detail.ImageSets) != 2 {
		t.Fatalf("expected 2 imageSets, got %d", len(detail.ImageSets))
	}

	isOne := detail.ImageSets[0]
	if isOne.Name != "is-one" {
		t.Errorf("expected first imageSet name is-one, got %s", isOne.Name)
	}
	if isOne.Total != 60 || isOne.Mirrored != 50 || isOne.Pending != 8 || isOne.Failed != 2 {
		t.Errorf("unexpected is-one counts: %+v", isOne)
	}

	// Resources should include IDMS, ITMS, and CatalogSource
	if len(detail.Resources) < 3 {
		t.Errorf("expected at least 3 resource links, got %d", len(detail.Resources))
	}
}

func TestTargetDetailNotFound(t *testing.T) {
	router := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/targets/nonexistent", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestUIServing(t *testing.T) {
	router := setupTestServer(t)

	// Root should redirect to /ui/
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Errorf("expected 301 redirect from /, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("expected redirect to /ui/, got %s", loc)
	}

	// /ui/ should serve the index.html
	req = httptest.NewRequest("GET", "/ui/", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for /ui/, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct == "" {
		t.Error("expected Content-Type header for /ui/")
	}
	body := rr.Body.String()
	if len(body) == 0 {
		t.Error("expected non-empty body for /ui/")
	}
	if !contains(body, "oc-mirror Dashboard") {
		t.Error("expected /ui/ to contain 'oc-mirror Dashboard'")
	}
}

func TestBuildResourceLinks(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"idms.yaml":                 "...",
			"itms.yaml":                 "...",
			"catalogsource-redhat.yaml": "...",
			"catalogsource-custom.yaml": "...",
		},
	}

	links := buildResourceLinks("my-target", cm)

	if len(links) != 4 {
		t.Fatalf("expected 4 links, got %d: %+v", len(links), links)
	}

	foundIDMS, foundITMS, foundCatalogs := false, false, 0
	for _, l := range links {
		switch l.Name {
		case "IDMS":
			foundIDMS = true
			if l.URL != "/api/v1/targets/my-target/resources/idms" {
				t.Errorf("unexpected IDMS URL: %s", l.URL)
			}
		case "ITMS":
			foundITMS = true
		default:
			foundCatalogs++
		}
	}
	if !foundIDMS {
		t.Error("missing IDMS link")
	}
	if !foundITMS {
		t.Error("missing ITMS link")
	}
	if foundCatalogs != 2 {
		t.Errorf("expected 2 catalog links, got %d", foundCatalogs)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
