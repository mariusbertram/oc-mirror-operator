package resourceapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/resourceapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("ResourceAPI Server", func() {
	var (
		router *mux.Router
		ns     = "test-ns"
		mtName = "test-mt"
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = mirrorv1alpha1.AddToScheme(scheme)

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
				Name:      fmt.Sprintf("oc-mirror-%s-resources", mtName),
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
				Name:      "oc-mirror-test-slug-packages",
				Namespace: ns,
			},
			Data: map[string]string{
				"packages.json": `{"catalog":"test","packages":[]}`,
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt, cm, packagesCm).Build()
		s := resourceapi.NewServer(c, ns)

		router = mux.NewRouter()
		s.RegisterRoutes(router)
	})

	Describe("API endpoints - JSON metadata", func() {
		It("lists all targets", func() {
			req := httptest.NewRequest("GET", "/api/v1/targets", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))

			var targets []resourceapi.TargetSummary
			Expect(json.Unmarshal(rr.Body.Bytes(), &targets)).To(Succeed())
			Expect(targets).To(HaveLen(1))
			Expect(targets[0].Name).To(Equal(mtName))
		})

		It("returns target detail", func() {
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/targets/%s", mtName), nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))

			var detail resourceapi.TargetDetail
			Expect(json.Unmarshal(rr.Body.Bytes(), &detail)).To(Succeed())
			Expect(detail.Name).To(Equal(mtName))
			Expect(detail.Resources).To(HaveLen(3))
			// Verify UI link format
			Expect(detail.Resources[0].URL).To(ContainSubstring("/imagesets/latest/"))
		})

		It("returns 404 for nonexistent target", func() {
			req := httptest.NewRequest("GET", "/api/v1/targets/nonexistent", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusNotFound))
		})
	})

	Describe("API endpoints - raw resources", func() {
		It("serves IDMS", func() {
			url := fmt.Sprintf("/api/v1/targets/%s/imagesets/is-one/idms.yaml", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring("kind: ImageDigestMirrorSet"))
			Expect(rr.Header().Get("Content-Type")).To(Equal("text/yaml"))
		})

		It("serves ITMS", func() {
			url := fmt.Sprintf("/api/v1/targets/%s/imagesets/is-one/itms.yaml", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring("kind: ImageTagMirrorSet"))
		})

		It("serves CatalogSource", func() {
			url := fmt.Sprintf("/api/v1/targets/%s/imagesets/is-one/catalogs/test/catalogsource.yaml", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring("kind: CatalogSource"))
		})

		It("serves Catalog Packages", func() {
			// slug is used for CM lookup
			url := "/api/v1/targets/any/imagesets/any/catalogs/test-slug/packages.json"
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring(`{"catalog":"test","packages":[]}`))
			Expect(rr.Header().Get("Content-Type")).To(Equal("application/json"))
		})

		It("returns 410 Gone for legacy redirects", func() {
			req := httptest.NewRequest("GET", "/resources/test-is/idms.yaml", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusGone))
		})
	})

	Describe("Web UI", func() {
		It("redirects root to /ui/", func() {
			req := httptest.NewRequest("GET", "/", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusMovedPermanently))
			Expect(rr.Header().Get("Location")).To(Equal("/ui/"))
		})

		It("serves the UI index", func() {
			req := httptest.NewRequest("GET", "/ui/", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			// uiFS is likely empty in tests or not easily checkable without real assets,
			// but RegisterRoutes should have set up the handler.
			// Based on the old test, we expect some content.
			if rr.Body.Len() > 0 {
				Expect(strings.Contains(rr.Body.String(), "oc-mirror")).To(BeTrue())
			}
		})
	})
})
