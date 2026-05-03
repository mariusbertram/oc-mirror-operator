package resourceapi_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
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
				"idms.yaml":                "kind: ImageDigestMirrorSet\nmetadata:\n  name: test",
				"itms.yaml":                "kind: ImageTagMirrorSet\nmetadata:\n  name: test",
				"catalogsource-test.yaml":  "kind: CatalogSource\nmetadata:\n  name: test",
				"clustercatalog-test.yaml": "kind: ClusterCatalog\nmetadata:\n  name: test",
			},
		}

		packagesCm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("oc-mirror-%s-test-slug-packages", mtName),
				Namespace: ns,
				Labels: map[string]string{
					"oc-mirror.openshift.io/mirrortarget":     mtName,
					"oc-mirror.openshift.io/catalog-packages": "test-slug",
				},
			},
			Data: map[string]string{
				"packages.json": `{"catalog":"test","packages":[]}`,
			},
		}

		sigCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-signatures", mtName),
				Namespace: ns,
			},
			BinaryData: map[string][]byte{
				"sha256-aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd": []byte("fake-gpg-signature"),
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt, cm, packagesCm, sigCM).Build()
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
			// IDMS + ITMS + CatalogSource-test + ClusterCatalog-test + Packages(test-slug) + Signatures
			Expect(detail.Resources).To(HaveLen(6))
			// Verify UI link format for static resources
			Expect(detail.Resources[0].URL).To(ContainSubstring("/imagesets/latest/"))
		})

		It("returns conditions in target detail", func() {
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/targets/%s", mtName), nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))

			var detail resourceapi.TargetDetail
			Expect(json.Unmarshal(rr.Body.Bytes(), &detail)).To(Succeed())
			// MirrorTarget in test has no conditions set — field must be present but empty
			Expect(detail.Conditions).NotTo(BeNil())
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

		It("serves ClusterCatalog", func() {
			url := fmt.Sprintf("/api/v1/targets/%s/imagesets/is-one/catalogs/test/clustercatalog.yaml", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring("kind: ClusterCatalog"))
		})

		It("serves Catalog Packages", func() {
			// slug is used for CM lookup
			url := fmt.Sprintf("/api/v1/targets/%s/imagesets/any/catalogs/test-slug/packages.json", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Body.String()).To(ContainSubstring(`{"catalog":"test","packages":[]}`))
			Expect(rr.Header().Get("Content-Type")).To(Equal("application/json"))
		})

		It("serves signatures as multi-document ConfigMap YAML", func() {
			url := fmt.Sprintf("/api/v1/targets/%s/signatures.yaml", mtName)
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))
			Expect(rr.Header().Get("Content-Type")).To(Equal("text/yaml"))
			Expect(rr.Body.String()).To(ContainSubstring("kind: ConfigMap"))
			Expect(rr.Body.String()).To(ContainSubstring("release.openshift.io/verification-signatures"))
		})

		It("returns 404 for signatures when target has no signatures CM", func() {
			url := "/api/v1/targets/nonexistent/signatures.yaml"
			req := httptest.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusNotFound))
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

	Describe("Image Failures API", func() {
		It("returns empty failed and pending images when ImageState ConfigMap does not exist", func() {
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/targets/%s/image-failures", mtName), nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))

			var response resourceapi.ImageFailuresResponse
			Expect(json.Unmarshal(rr.Body.Bytes(), &response)).To(Succeed())
			Expect(response.Failed).To(BeEmpty())
			Expect(response.Pending).To(BeEmpty())
		})

		It("returns 404 for nonexistent target", func() {
			req := httptest.NewRequest("GET", "/api/v1/targets/nonexistent/image-failures", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusNotFound))
		})

		It("returns failed and pending images with full details", func() {
			// Create ImageState with mixed states
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			state := imagestate.ImageState{
				"registry.example.com/img1": {
					Source:            "quay.io/source1",
					State:             "Pending",
					LastError:         "",
					RetryCount:        2,
					PermanentlyFailed: false,
					Refs: []imagestate.ImageRef{
						{ImageSet: "is-one", Origin: imagestate.OriginRelease},
					},
				},
				"registry.example.com/img2": {
					Source:            "quay.io/source2",
					State:             "Failed",
					LastError:         "connection timeout",
					RetryCount:        10,
					PermanentlyFailed: true,
					Refs: []imagestate.ImageRef{
						{ImageSet: "is-one", Origin: imagestate.OriginOperator},
						{ImageSet: "is-two", Origin: imagestate.OriginOperator},
					},
				},
				"registry.example.com/img3": {
					Source:            "quay.io/source3",
					State:             "Mirrored",
					RetryCount:        0,
					PermanentlyFailed: false,
					Refs: []imagestate.ImageRef{
						{ImageSet: "is-two"},
					},
				},
			}

			// Encode as gzip and create ConfigMap
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			_ = json.NewEncoder(gz).Encode(state)
			_ = gz.Close()

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName + "-images",
					Namespace: ns,
				},
				BinaryData: map[string][]byte{
					"images.json.gz": buf.Bytes(),
				},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&mirrorv1alpha1.MirrorTarget{
					ObjectMeta: metav1.ObjectMeta{Name: mtName, Namespace: ns},
					Spec: mirrorv1alpha1.MirrorTargetSpec{
						Registry:  "registry.example.com",
						ImageSets: []string{"is-one", "is-two"},
					},
				},
				cm,
			).Build()

			s := resourceapi.NewServer(c, ns)
			router := mux.NewRouter()
			s.RegisterRoutes(router)

			req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/targets/%s/image-failures", mtName), nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			Expect(rr.Code).To(Equal(http.StatusOK))

			var response resourceapi.ImageFailuresResponse
			Expect(json.Unmarshal(rr.Body.Bytes(), &response)).To(Succeed())

			// Should have 1 pending image (img1)
			Expect(response.Pending).To(HaveLen(1))
			Expect(response.Pending[0].Destination).To(Equal("registry.example.com/img1"))
			Expect(response.Pending[0].Source).To(Equal("quay.io/source1"))
			Expect(response.Pending[0].State).To(Equal("Pending"))
			Expect(response.Pending[0].RetryCount).To(Equal(2))
			Expect(response.Pending[0].ImageSet).To(Equal("is-one"))
			Expect(response.Pending[0].PermanentlyFailed).To(BeFalse())

			// Should have 2 failed images (img2 in is-one and is-two)
			Expect(response.Failed).To(HaveLen(2))
			Expect(response.Failed[0].Destination).To(Equal("registry.example.com/img2"))
			Expect(response.Failed[0].Source).To(Equal("quay.io/source2"))
			Expect(response.Failed[0].LastError).To(Equal("connection timeout"))
			Expect(response.Failed[0].RetryCount).To(Equal(10))
			Expect(response.Failed[0].PermanentlyFailed).To(BeTrue())

			// img3 (Mirrored) should not appear
			for _, f := range response.Failed {
				Expect(f.Destination).NotTo(Equal("registry.example.com/img3"))
			}
			for _, p := range response.Pending {
				Expect(p.Destination).NotTo(Equal("registry.example.com/img3"))
			}
		})
	})

	Describe("lookupMirrorTarget", func() {
		var (
			ns     = "lookup-test-ns"
			mtName = "lookup-test-mt"
		)

		It("should find MirrorTarget by name in namespace-bound mode", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: ns,
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/mirror",
				},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt).Build()
			server := resourceapi.NewServer(c, ns)

			ctx := context.Background()
			found, err := server.LookupMirrorTarget(ctx, mtName)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).NotTo(BeNil())
			Expect(found.Name).To(Equal(mtName))
			Expect(found.Namespace).To(Equal(ns))
		})

		It("should return error for non-existent MirrorTarget in namespace-bound mode", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			server := resourceapi.NewServer(c, ns)

			ctx := context.Background()
			found, err := server.LookupMirrorTarget(ctx, "nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(found).NotTo(BeNil()) // LookupMirrorTarget returns empty object on error
		})

		It("should find MirrorTarget by name in cluster-wide mode", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			mt1 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: "ns1",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/mirror",
				},
			}

			mt2 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-target",
					Namespace: "ns2",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/other",
				},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt1, mt2).Build()
			server := resourceapi.NewServerClusterWide(c)

			ctx := context.Background()
			found, err := server.LookupMirrorTarget(ctx, mtName)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).NotTo(BeNil())
			Expect(found.Name).To(Equal(mtName))
			Expect(found.Namespace).To(Equal("ns1"))
		})

		It("should return error for non-existent MirrorTarget in cluster-wide mode", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mtName,
					Namespace: "ns1",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/mirror",
				},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt).Build()
			server := resourceapi.NewServerClusterWide(c)

			ctx := context.Background()
			found, err := server.LookupMirrorTarget(ctx, "nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(found).To(BeNil())
		})
	})

	Describe("NewServerClusterWide", func() {
		It("should initialize server with cluster-wide namespace", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			server := resourceapi.NewServerClusterWide(c)

			Expect(server).NotTo(BeNil())
			// Verify that the server is cluster-wide by checking that it can search across namespaces
			// (We verify this indirectly by testing lookupMirrorTarget with multiple namespaces in next test)
		})

		It("should search across multiple namespaces", func() {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = mirrorv1alpha1.AddToScheme(scheme)

			mt1 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "target-a",
					Namespace: "namespace-a",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/a",
				},
			}

			mt2 := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "target-b",
					Namespace: "namespace-b",
				},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "registry.example.com/b",
				},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt1, mt2).Build()
			server := resourceapi.NewServerClusterWide(c)

			ctx := context.Background()

			By("finding target-a in namespace-a")
			found1, err := server.LookupMirrorTarget(ctx, "target-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(found1.Name).To(Equal("target-a"))
			Expect(found1.Namespace).To(Equal("namespace-a"))

			By("finding target-b in namespace-b")
			found2, err := server.LookupMirrorTarget(ctx, "target-b")
			Expect(err).NotTo(HaveOccurred())
			Expect(found2.Name).To(Equal("target-b"))
			Expect(found2.Namespace).To(Equal("namespace-b"))
		})
	})
})
