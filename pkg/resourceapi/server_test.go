package resourceapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResourceAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := "test-ns"
	mtName := "test-mt"
	cmName := fmt.Sprintf("oc-mirror-%s-resources", mtName)
	packagesCmName := "oc-mirror-test-slug-packages"

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

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, packagesCm).Build()
	s := NewServer(c, ns)

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
			req, _ := http.NewRequest("GET", tt.url, nil)
			rr := httptest.NewRecorder()

			// Use mux router to handle variables
			r := mux.NewRouter()
			api := r.PathPrefix("/api/v1").Subrouter()
			api.HandleFunc("/targets/{mt}/resources/idms", s.handleIDMS)
			api.HandleFunc("/targets/{mt}/resources/itms", s.handleITMS)
			api.HandleFunc("/targets/{mt}/resources/catalogs/{slug}/catalogsource", s.handleCatalogSource)
			api.HandleFunc("/targets/{mt}/resources/catalogs/{slug}/packages", s.handleCatalogPackages)
			r.PathPrefix("/resources/{imageset}/").HandlerFunc(s.handleLegacyRedirect)

			r.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
			if tt.expectedBody != "" && rr.Body.String()[:len(tt.expectedBody)] != tt.expectedBody {
				t.Errorf("expected body to start with %q, got %q", tt.expectedBody, rr.Body.String())
			}
		})
	}
}
