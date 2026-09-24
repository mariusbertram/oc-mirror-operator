package resourceapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/resourceapi"
)

const bundlesURL = "/api/v1/imagesets/ns/is/catalogs/redhat-operator-index-v4.12/packages"

func newBundlesServer(t *testing.T) (http.Handler, client.Client) {
	t.Helper()
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	_ = mirrorv1alpha1.AddToScheme(sc)
	is := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is", Namespace: "ns"},
		Spec: mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{Operators: []mirrorv1alpha1.Operator{{
			Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.12",
			IncludeConfig: mirrorv1alpha1.IncludeConfig{Packages: []mirrorv1alpha1.IncludePackage{
				{Name: "op", Bundles: []mirrorv1alpha1.SelectedBundle{{Name: "op.v1.0.0"}}},
			}},
		}}}},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(is).Build()
	r := mux.NewRouter()
	resourceapi.NewServerForTest(c, "ns").RegisterAPIRoutes(r)
	return r, c
}

func patchPackages(t *testing.T, h http.Handler, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, bundlesURL, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d: %s", rr.Code, rr.Body.String())
	}
}

func opPackages(t *testing.T, c client.Client) []mirrorv1alpha1.IncludePackage {
	t.Helper()
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "is"}, is); err != nil {
		t.Fatal(err)
	}
	return is.Spec.Mirror.Operators[0].Packages
}

func TestPackagesAPI_ReturnsBundles(t *testing.T) {
	h, _ := newBundlesServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, bundlesURL, nil))
	var got []struct {
		Name    string   `json:"name"`
		Bundles []string `json:"bundles"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	if len(got) != 1 || len(got[0].Bundles) != 1 || got[0].Bundles[0] != "op.v1.0.0" {
		t.Fatalf("GET = %+v", got)
	}
}

func TestPackagesAPI_PatchBundles(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"absent field keeps the selection", `{"packages":[{"name":"op"}],"exclude":[]}`, []string{"op.v1.0.0"}},
		{"legacy include keeps the selection", `{"include":["op"],"exclude":[]}`, []string{"op.v1.0.0"}},
		{"explicit list replaces it", `{"packages":[{"name":"op","bundles":["op.v2.0.0"]}],"exclude":[]}`, []string{"op.v2.0.0"}},
		{"explicit empty list clears it", `{"packages":[{"name":"op","bundles":[]}],"exclude":[]}`, nil},
		{"version constraints drop it", `{"packages":[{"name":"op","minVersion":"1.0.0"}],"exclude":[]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, c := newBundlesServer(t)
			patchPackages(t, h, tt.body)
			pkgs := opPackages(t, c)
			got := make([]string, 0, len(pkgs[0].Bundles))
			for _, b := range pkgs[0].Bundles {
				got = append(got, b.Name)
			}
			if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
				t.Fatalf("bundles = %v, want %v", got, tt.want)
			}
		})
	}
}
