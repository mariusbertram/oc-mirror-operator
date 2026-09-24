package resourceapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// Mutating requests without a Bearer token must never fall back to the
// server's own service-account client (#141).
func TestRequireTokenForWrites(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	_ = mirrorv1alpha1.AddToScheme(sc)
	s := NewServerForTest(fake.NewClientBuilder().WithScheme(sc).Build(), "ns")
	r := mux.NewRouter()
	s.RegisterAPIRoutes(r)

	tests := []struct {
		name, method, path, header, value string
		wantUnauthorized                  bool
	}{
		{"patch without token", http.MethodPatch, "/api/v1/imagesets/ns/is/additional-images", "", "", true},
		{"delete without token", http.MethodDelete, "/api/v1/imagesets/ns/is", "", "", true},
		{"patch spec without token", http.MethodPatch, "/api/v1/targets/ns/mt/spec", "", "", true},
		{"empty bearer", http.MethodPatch, "/api/v1/imagesets/ns/is/recollect", "Authorization", "Bearer ", true},
		{"patch with bearer token", http.MethodPatch, "/api/v1/imagesets/ns/is/recollect", "Authorization", "Bearer t", false},
		{"patch with forwarded token", http.MethodPatch, "/api/v1/imagesets/ns/is/recollect", "X-Forwarded-Access-Token", "t", false},
		{"read without token", http.MethodGet, "/api/v1/targets", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if got := rr.Code == http.StatusUnauthorized; got != tt.wantUnauthorized {
				t.Errorf("status = %d, want unauthorized=%v", rr.Code, tt.wantUnauthorized)
			}
		})
	}
}
