package resources

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// encodeImageState gzip-compresses an ImageState for use in test ConfigMaps.
func encodeImageState(state imagestate.ImageState) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_ = json.NewEncoder(gz).Encode(state)
	_ = gz.Close()
	return buf.Bytes()
}

// newTestMux creates a ServeMux with the same routing as Server.Run.
func newTestMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/resources/", srv.handleIndex)
	mux.HandleFunc("/resources/{imageset}/idms.yaml", srv.handleIDMS)
	mux.HandleFunc("/resources/{imageset}/itms.yaml", srv.handleITMS)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/catalogsource.yaml", srv.handleCatalogSource)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/clustercatalog.yaml", srv.handleClusterCatalog)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/packages.json", srv.handleCatalogPackages)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/upstream-packages.json", srv.handleUpstreamCatalogPackages)
	mux.HandleFunc("/resources/{imageset}/signature-configmaps.yaml", srv.handleSignatures)
	return mux
}

// --- Pure function tests ---

var _ = Describe("isImageSetReady", func() {
	It("returns true when Ready condition is True", func() {
		is := &mirrorv1alpha1.ImageSet{
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()},
				},
			},
		}
		Expect(isImageSetReady(is)).To(BeTrue())
	})

	It("returns false when Ready condition is False", func() {
		is := &mirrorv1alpha1.ImageSet{
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotReady", LastTransitionTime: metav1.Now()},
				},
			},
		}
		Expect(isImageSetReady(is)).To(BeFalse())
	})

	It("returns false when no conditions exist", func() {
		is := &mirrorv1alpha1.ImageSet{}
		Expect(isImageSetReady(is)).To(BeFalse())
	})

	It("returns false when Ready is not among conditions", func() {
		is := &mirrorv1alpha1.ImageSet{
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "CatalogReady", Status: metav1.ConditionTrue, Reason: "Built", LastTransitionTime: metav1.Now()},
				},
			},
		}
		Expect(isImageSetReady(is)).To(BeFalse())
	})
})

var _ = Describe("isCatalogReady", func() {
	It("returns true when CatalogReady is True", func() {
		is := &mirrorv1alpha1.ImageSet{
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "CatalogReady", Status: metav1.ConditionTrue, Reason: "Built", LastTransitionTime: metav1.Now()},
				},
			},
		}
		Expect(isCatalogReady(is)).To(BeTrue())
	})

	It("returns false when CatalogReady is False", func() {
		is := &mirrorv1alpha1.ImageSet{
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "CatalogReady", Status: metav1.ConditionFalse, Reason: "Building", LastTransitionTime: metav1.Now()},
				},
			},
		}
		Expect(isCatalogReady(is)).To(BeFalse())
	})

	It("returns false when no conditions exist", func() {
		is := &mirrorv1alpha1.ImageSet{}
		Expect(isCatalogReady(is)).To(BeFalse())
	})
})

var _ = Describe("extractCatalogs", func() {
	It("extracts catalogs from operators spec", func() {
		is := &mirrorv1alpha1.ImageSet{
			Spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21"},
					},
				},
			},
		}
		catalogs := extractCatalogs(is, "mirror.example.com")
		Expect(catalogs).To(HaveLen(1))
		Expect(catalogs[0].SourceCatalog).To(Equal("registry.redhat.io/redhat/redhat-operator-index:v4.21"))
		Expect(catalogs[0].TargetImage).To(Equal("mirror.example.com/redhat/redhat-operator-index:v4.21"))
		Expect(catalogs[0].DisplayName).To(Equal("OC Mirror - redhat-operator-index"))
	})

	It("skips operators with empty catalog", func() {
		is := &mirrorv1alpha1.ImageSet{
			Spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{Catalog: ""},
						{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21"},
					},
				},
			},
		}
		catalogs := extractCatalogs(is, "mirror.example.com")
		Expect(catalogs).To(HaveLen(1))
	})

	It("returns empty for no operators", func() {
		is := &mirrorv1alpha1.ImageSet{
			Spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{},
			},
		}
		catalogs := extractCatalogs(is, "mirror.example.com")
		Expect(catalogs).To(BeEmpty())
	})
})

var _ = Describe("CatalogTargetRef", func() {
	It("derives target from source catalog with tag", func() {
		op := mirrorv1alpha1.Operator{
			Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat/redhat-operator-index:v4.21"))
	})

	It("uses TargetCatalog when set", func() {
		op := mirrorv1alpha1.Operator{
			Catalog:       "registry.redhat.io/redhat/redhat-operator-index:v4.21",
			TargetCatalog: "custom/catalog",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/custom/catalog:v4.21"))
	})

	It("uses TargetTag when set", func() {
		op := mirrorv1alpha1.Operator{
			Catalog:   "registry.redhat.io/redhat/redhat-operator-index:v4.21",
			TargetTag: "custom-tag",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat/redhat-operator-index:custom-tag"))
	})

	It("uses 'latest' when source has no tag or digest", func() {
		op := mirrorv1alpha1.Operator{
			Catalog: "registry.redhat.io/redhat/redhat-operator-index",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat/redhat-operator-index:latest"))
	})

	It("handles digest-only catalog reference with latest fallback", func() {
		op := mirrorv1alpha1.Operator{
			Catalog: "registry.redhat.io/redhat/redhat-operator-index@sha256:abcdef1234567890",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat/redhat-operator-index:latest"))
	})

	It("handles tag+digest catalog reference using the tag", func() {
		op := mirrorv1alpha1.Operator{
			Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21@sha256:abcdef1234567890",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat/redhat-operator-index:v4.21"))
	})

	It("uses TargetCatalog and TargetTag together", func() {
		op := mirrorv1alpha1.Operator{
			Catalog:       "registry.redhat.io/redhat/redhat-operator-index:v4.21",
			TargetCatalog: "my-catalog",
			TargetTag:     "v1.0",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/my-catalog:v1.0"))
	})

	It("strips registry from single-segment catalog path", func() {
		op := mirrorv1alpha1.Operator{
			Catalog: "redhat-operator-index:v4.21",
		}
		Expect(CatalogTargetRef("mirror.example.com", op)).To(Equal("mirror.example.com/redhat-operator-index:v4.21"))
	})
})

var _ = Describe("catalogDisplayName", func() {
	It("extracts last path segment", func() {
		Expect(catalogDisplayName("registry.redhat.io/redhat/redhat-operator-index:v4.21")).
			To(Equal("OC Mirror - redhat-operator-index"))
	})

	It("handles single-segment path", func() {
		Expect(catalogDisplayName("my-catalog:v1.0")).To(Equal("OC Mirror - my-catalog"))
	})

	It("handles bare name", func() {
		Expect(catalogDisplayName("my-catalog")).To(Equal("OC Mirror - my-catalog"))
	})
})

var _ = Describe("CatalogSlug", func() {
	It("extracts last path segment as slug", func() {
		Expect(CatalogSlug("registry.redhat.io/redhat/redhat-operator-index:v4.21")).
			To(Equal("redhat-operator-index"))
	})

	It("handles single-segment path", func() {
		Expect(CatalogSlug("my-catalog:v1.0")).To(Equal("my-catalog"))
	})
})

var _ = Describe("FindCatalog", func() {
	catalogs := []CatalogInfo{
		{SourceCatalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21"},
		{SourceCatalog: "registry.redhat.io/redhat/certified-operator-index:v4.21"},
	}

	It("finds a catalog by slug", func() {
		cat, ok := FindCatalog(catalogs, "redhat-operator-index")
		Expect(ok).To(BeTrue())
		Expect(cat.SourceCatalog).To(Equal("registry.redhat.io/redhat/redhat-operator-index:v4.21"))
	})

	It("returns false when slug not found", func() {
		_, ok := FindCatalog(catalogs, "nonexistent")
		Expect(ok).To(BeFalse())
	})

	It("returns false for empty catalog list", func() {
		_, ok := FindCatalog(nil, "anything")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("BuildCatalogPackagesResponse", func() {
	It("builds complete response from DeclarativeConfig", func() {
		cat := CatalogInfo{
			SourceCatalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21",
			TargetImage:   "mirror.example.com/redhat-operator-index:v4.21",
		}
		cfg := &declcfg.DeclarativeConfig{
			Packages: []declcfg.Package{
				{Name: "my-operator", DefaultChannel: "stable"},
			},
			Channels: []declcfg.Channel{
				{
					Package: "my-operator",
					Name:    "stable",
					Entries: []declcfg.ChannelEntry{
						{Name: "my-operator.v1.0.0"},
						{Name: "my-operator.v1.1.0"},
					},
				},
			},
			Bundles: []declcfg.Bundle{
				{
					Name:    "my-operator.v1.0.0",
					Package: "my-operator",
					Properties: []property.Property{
						{Type: "olm.package", Value: json.RawMessage(`{"packageName":"my-operator","version":"1.0.0"}`)},
					},
				},
				{
					Name:    "my-operator.v1.1.0",
					Package: "my-operator",
					Properties: []property.Property{
						{Type: "olm.package", Value: json.RawMessage(`{"packageName":"my-operator","version":"1.1.0"}`)},
					},
				},
			},
		}

		resp := BuildCatalogPackagesResponse(cat, cfg)
		Expect(resp.Catalog).To(Equal("registry.redhat.io/redhat/redhat-operator-index:v4.21"))
		Expect(resp.TargetImage).To(Equal("mirror.example.com/redhat-operator-index:v4.21"))
		Expect(resp.Packages).To(HaveLen(1))
		Expect(resp.Packages[0].Name).To(Equal("my-operator"))
		Expect(resp.Packages[0].DefaultChannel).To(Equal("stable"))
		Expect(resp.Packages[0].Channels).To(HaveLen(1))
		Expect(resp.Packages[0].Channels[0].Name).To(Equal("stable"))
		Expect(resp.Packages[0].Channels[0].Entries).To(HaveLen(2))
		// Entries are sorted by name
		Expect(resp.Packages[0].Channels[0].Entries[0].Name).To(Equal("my-operator.v1.0.0"))
		Expect(resp.Packages[0].Channels[0].Entries[0].Version).To(Equal("1.0.0"))
		Expect(resp.Packages[0].Channels[0].Entries[1].Name).To(Equal("my-operator.v1.1.0"))
		Expect(resp.Packages[0].Channels[0].Entries[1].Version).To(Equal("1.1.0"))
	})

	It("handles empty DeclarativeConfig", func() {
		resp := BuildCatalogPackagesResponse(CatalogInfo{SourceCatalog: "src", TargetImage: "tgt"}, &declcfg.DeclarativeConfig{})
		Expect(resp.Packages).To(BeEmpty())
	})

	It("sorts packages alphabetically", func() {
		cfg := &declcfg.DeclarativeConfig{
			Packages: []declcfg.Package{
				{Name: "z-operator"},
				{Name: "a-operator"},
			},
		}
		resp := BuildCatalogPackagesResponse(CatalogInfo{SourceCatalog: "src", TargetImage: "tgt"}, cfg)
		Expect(resp.Packages).To(HaveLen(2))
		Expect(resp.Packages[0].Name).To(Equal("a-operator"))
		Expect(resp.Packages[1].Name).To(Equal("z-operator"))
	})

	It("sorts channels alphabetically within a package", func() {
		cfg := &declcfg.DeclarativeConfig{
			Packages: []declcfg.Package{{Name: "op"}},
			Channels: []declcfg.Channel{
				{Package: "op", Name: "stable"},
				{Package: "op", Name: "alpha"},
			},
		}
		resp := BuildCatalogPackagesResponse(CatalogInfo{SourceCatalog: "s", TargetImage: "t"}, cfg)
		Expect(resp.Packages[0].Channels[0].Name).To(Equal("alpha"))
		Expect(resp.Packages[0].Channels[1].Name).To(Equal("stable"))
	})
})

var _ = Describe("bundleVersion", func() {
	It("extracts version from olm.package property", func() {
		b := declcfg.Bundle{
			Properties: []property.Property{
				{Type: "olm.package", Value: json.RawMessage(`{"packageName":"my-op","version":"1.2.3"}`)},
			},
		}
		Expect(bundleVersion(b)).To(Equal("1.2.3"))
	})

	It("returns empty for non-olm.package properties", func() {
		b := declcfg.Bundle{
			Properties: []property.Property{
				{Type: "olm.gvk", Value: json.RawMessage(`{"group":"example.com"}`)},
			},
		}
		Expect(bundleVersion(b)).To(BeEmpty())
	})

	It("returns empty for no properties", func() {
		Expect(bundleVersion(declcfg.Bundle{})).To(BeEmpty())
	})

	It("returns empty for invalid JSON in olm.package", func() {
		b := declcfg.Bundle{
			Properties: []property.Property{
				{Type: "olm.package", Value: json.RawMessage(`{invalid}`)},
			},
		}
		Expect(bundleVersion(b)).To(BeEmpty())
	})

	It("returns empty when version field is empty string", func() {
		b := declcfg.Bundle{
			Properties: []property.Property{
				{Type: "olm.package", Value: json.RawMessage(`{"packageName":"op","version":""}`)},
			},
		}
		Expect(bundleVersion(b)).To(BeEmpty())
	})

	It("skips non-matching properties and finds olm.package", func() {
		b := declcfg.Bundle{
			Properties: []property.Property{
				{Type: "olm.gvk", Value: json.RawMessage(`{}`)},
				{Type: "olm.package", Value: json.RawMessage(`{"version":"2.0.0"}`)},
			},
		}
		Expect(bundleVersion(b)).To(Equal("2.0.0"))
	})
})

// --- Server Handler tests ---

var _ = Describe("Server handlers", func() {
	var (
		testScheme *runtime.Scheme
	)

	BeforeEach(func() {
		testScheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(testScheme)).To(Succeed())
		Expect(mirrorv1alpha1.AddToScheme(testScheme)).To(Succeed())
	})

	setupServer := func(objects ...client.Object) *http.ServeMux {
		c := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(objects...).
			Build()
		srv := NewServer(c, "default", "test-target", "")
		return newTestMux(srv)
	}

	newMirrorTarget := func() *mirrorv1alpha1.MirrorTarget {
		return &mirrorv1alpha1.MirrorTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "default"},
			Spec: mirrorv1alpha1.MirrorTargetSpec{
				Registry:   "mirror.example.com",
				ImageSets:  []string{"my-imageset"},
				AuthSecret: "my-pull-secret",
			},
		}
	}

	newReadyImageSet := func() *mirrorv1alpha1.ImageSet {
		return &mirrorv1alpha1.ImageSet{
			ObjectMeta: metav1.ObjectMeta{Name: "my-imageset", Namespace: "default"},
			Spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.21"},
					},
				},
			},
			Status: mirrorv1alpha1.ImageSetStatus{
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue, Reason: "MirrorComplete", LastTransitionTime: metav1.Now()},
				},
			},
		}
	}

	newImageStateConfigMap := func() *corev1.ConfigMap {
		state := imagestate.ImageState{
			"mirror.example.com/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123def456": &imagestate.ImageEntry{
				Source: "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123def456",
				State:  "Mirrored",
			},
			"mirror.example.com/openshift-release-dev/ocp-release:4.14.1-x86_64": &imagestate.ImageEntry{
				Source: "quay.io/openshift-release-dev/ocp-release:4.14.1-x86_64",
				State:  "Mirrored",
			},
		}
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "my-imageset-images", Namespace: "default"},
			BinaryData: map[string][]byte{"images.json.gz": encodeImageState(state)},
		}
	}

	newSignatureConfigMap := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "my-imageset-signatures", Namespace: "default"},
			BinaryData: map[string][]byte{
				"sha256-aabbccddee112233445566778899aabbccddee112233445566778899aabbccdd": []byte("fake-gpg-sig"),
			},
		}
	}

	// -- NewServer --

	Describe("NewServer", func() {
		It("creates a server with the correct fields", func() {
			c := fake.NewClientBuilder().WithScheme(testScheme).Build()
			srv := NewServer(c, "test-ns", "my-target", "/auth/path")
			Expect(srv.namespace).To(Equal("test-ns"))
			Expect(srv.target).To(Equal("my-target"))
			Expect(srv.authConfigPath).To(Equal("/auth/path"))
			Expect(srv.client).NotTo(BeNil())
		})
	})

	// -- handleIndex --

	Describe("handleIndex", func() {
		It("returns JSON index with ImageSets and catalog resources", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet, "/resources/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("application/json"))

			var idx resourceIndex
			Expect(json.Unmarshal(rec.Body.Bytes(), &idx)).To(Succeed())
			Expect(idx.ImageSets).To(HaveLen(1))
			Expect(idx.ImageSets[0].Name).To(Equal("my-imageset"))
			Expect(idx.ImageSets[0].Ready).To(BeTrue())
			Expect(idx.ImageSets[0].Resources).To(ContainElement("/resources/my-imageset/idms.yaml"))
			Expect(idx.ImageSets[0].Resources).To(ContainElement("/resources/my-imageset/itms.yaml"))
			Expect(idx.ImageSets[0].Resources).To(ContainElement("/resources/my-imageset/signature-configmaps.yaml"))
			Expect(idx.ImageSets[0].Catalogs).To(ContainElement("redhat-operator-index"))
		})

		It("returns 404 for non-exact resource paths", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet, "/resources/nonexistent", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 500 when MirrorTarget does not exist", func() {
			mux := setupServer() // no objects

			req := httptest.NewRequest(http.MethodGet, "/resources/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
		})

		It("returns empty ImageSets when MirrorTarget has no ImageSets", func() {
			mt := newMirrorTarget()
			mt.Spec.ImageSets = nil
			mux := setupServer(mt)

			req := httptest.NewRequest(http.MethodGet, "/resources/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			var idx resourceIndex
			Expect(json.Unmarshal(rec.Body.Bytes(), &idx)).To(Succeed())
			Expect(idx.ImageSets).To(BeEmpty())
		})

		It("marks ImageSets without Ready condition as not ready", func() {
			is := newReadyImageSet()
			is.Status.Conditions = nil
			mux := setupServer(newMirrorTarget(), is)

			req := httptest.NewRequest(http.MethodGet, "/resources/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			var idx resourceIndex
			Expect(json.Unmarshal(rec.Body.Bytes(), &idx)).To(Succeed())
			Expect(idx.ImageSets).To(HaveLen(1))
			Expect(idx.ImageSets[0].Ready).To(BeFalse())
		})

		It("sorts multiple ImageSets alphabetically", func() {
			mt := newMirrorTarget()
			mt.Spec.ImageSets = []string{"z-imageset", "a-imageset"}

			isA := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "a-imageset", Namespace: "default"},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			isZ := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "z-imageset", Namespace: "default"},
				Spec:       mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{}},
			}
			mux := setupServer(mt, isA, isZ)

			req := httptest.NewRequest(http.MethodGet, "/resources/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			var idx resourceIndex
			Expect(json.Unmarshal(rec.Body.Bytes(), &idx)).To(Succeed())
			Expect(idx.ImageSets).To(HaveLen(2))
			Expect(idx.ImageSets[0].Name).To(Equal("a-imageset"))
			Expect(idx.ImageSets[1].Name).To(Equal("z-imageset"))
		})

		It("handles direct call to handleIndex with /resources path (no trailing slash)", func() {
			c := fake.NewClientBuilder().WithScheme(testScheme).
				WithObjects(newMirrorTarget(), newReadyImageSet()).Build()
			srv := NewServer(c, "default", "test-target", "")

			req := httptest.NewRequest(http.MethodGet, "/resources", nil)
			rec := httptest.NewRecorder()
			srv.handleIndex(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
		})
	})

	// -- handleIDMS --

	Describe("handleIDMS", func() {
		It("returns IDMS YAML for a ready ImageSet", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet(), newImageStateConfigMap())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/idms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			Expect(rec.Body.String()).To(ContainSubstring("ImageDigestMirrorSet"))
		})

		It("returns 409 when ImageSet is not ready", func() {
			is := newReadyImageSet()
			is.Status.Conditions = []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending", LastTransitionTime: metav1.Now()},
			}
			mux := setupServer(newMirrorTarget(), is, newImageStateConfigMap())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/idms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusConflict))
			Expect(rec.Body.String()).To(ContainSubstring("not ready"))
		})

		It("returns 404 when ImageSet does not exist in cluster", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/idms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when ImageSet is not in MirrorTarget's list", func() {
			mt := newMirrorTarget()
			mt.Spec.ImageSets = []string{"other-imageset"}
			mux := setupServer(mt, newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/idms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})
	})

	// -- handleITMS --

	Describe("handleITMS", func() {
		It("returns ITMS YAML for a ready ImageSet", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet(), newImageStateConfigMap())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/itms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			Expect(rec.Body.String()).To(ContainSubstring("ImageTagMirrorSet"))
		})

		It("returns 409 when ImageSet is not ready", func() {
			is := newReadyImageSet()
			is.Status.Conditions = []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending", LastTransitionTime: metav1.Now()},
			}
			mux := setupServer(newMirrorTarget(), is)

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/itms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusConflict))
			Expect(rec.Body.String()).To(ContainSubstring("not ready"))
		})

		It("returns 404 when ImageSet does not exist", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet, "/resources/my-imageset/itms.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})
	})

	// -- handleCatalogSource --

	Describe("handleCatalogSource", func() {
		It("returns CatalogSource YAML for a valid catalog", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/catalogsource.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			body := rec.Body.String()
			Expect(body).To(ContainSubstring("CatalogSource"))
			Expect(body).To(ContainSubstring("oc-mirror-redhat-operator-index"))
			Expect(body).To(ContainSubstring("openshift-marketplace"))
			Expect(body).To(ContainSubstring("my-pull-secret"))
		})

		It("returns 404 when ImageSet does not exist", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/catalogsource.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when catalog slug is not found", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/nonexistent-catalog/catalogsource.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
			Expect(rec.Body.String()).To(ContainSubstring("not found"))
		})
	})

	// -- handleClusterCatalog --

	Describe("handleClusterCatalog", func() {
		It("returns ClusterCatalog YAML for a valid catalog", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/clustercatalog.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			body := rec.Body.String()
			Expect(body).To(ContainSubstring("ClusterCatalog"))
			Expect(body).To(ContainSubstring("oc-mirror-redhat-operator-index"))
		})

		It("returns 404 when ImageSet does not exist", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/clustercatalog.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when catalog slug is not found", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/nonexistent/clustercatalog.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})
	})

	// -- handleCatalogPackages --

	Describe("handleCatalogPackages", func() {
		It("returns 404 when ImageSet does not exist", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/packages.json", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 409 when catalog is not ready", func() {
			// ImageSet exists but only has Ready, not CatalogReady
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/packages.json", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusConflict))
			Expect(rec.Body.String()).To(ContainSubstring("not ready"))
		})

		It("returns 404 when catalog slug is not found", func() {
			is := newReadyImageSet()
			is.Status.Conditions = append(is.Status.Conditions, metav1.Condition{
				Type: "CatalogReady", Status: metav1.ConditionTrue, Reason: "Built", LastTransitionTime: metav1.Now(),
			})
			mux := setupServer(newMirrorTarget(), is)

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/nonexistent/packages.json", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
			Expect(rec.Body.String()).To(ContainSubstring("not found"))
		})

		It("returns 500 when FBC loading fails (unreachable registry)", func() {
			// Use 127.0.0.1:1 which will refuse connections immediately.
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "127.0.0.1:1/mirror",
					Insecure:  true,
					ImageSets: []string{"my-imageset"},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "my-imageset", Namespace: "default"},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "127.0.0.1:1/redhat/redhat-operator-index:v4.21"},
						},
					},
				},
				Status: mirrorv1alpha1.ImageSetStatus{
					Conditions: []metav1.Condition{
						{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Done", LastTransitionTime: metav1.Now()},
						{Type: "CatalogReady", Status: metav1.ConditionTrue, Reason: "Built", LastTransitionTime: metav1.Now()},
					},
				},
			}
			mux := setupServer(mt, is)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/packages.json", nil)
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
			Expect(rec.Body.String()).To(ContainSubstring("failed to load catalog"))
		})
	})

	// -- handleUpstreamCatalogPackages --

	Describe("handleUpstreamCatalogPackages", func() {
		It("returns 404 when ImageSet does not exist", func() {
			mux := setupServer(newMirrorTarget())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/upstream-packages.json", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 404 when catalog slug is not found", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/nonexistent/upstream-packages.json", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
			Expect(rec.Body.String()).To(ContainSubstring("not found"))
		})

		It("returns 500 when FBC loading fails (unreachable catalog)", func() {
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "default"},
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry:  "127.0.0.1:1",
					ImageSets: []string{"my-imageset"},
				},
			}
			is := &mirrorv1alpha1.ImageSet{
				ObjectMeta: metav1.ObjectMeta{Name: "my-imageset", Namespace: "default"},
				Spec: mirrorv1alpha1.ImageSetSpec{
					Mirror: mirrorv1alpha1.Mirror{
						Operators: []mirrorv1alpha1.Operator{
							{Catalog: "127.0.0.1:1/redhat/redhat-operator-index:v4.21"},
						},
					},
				},
				Status: mirrorv1alpha1.ImageSetStatus{
					Conditions: []metav1.Condition{
						{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Done", LastTransitionTime: metav1.Now()},
					},
				},
			}
			mux := setupServer(mt, is)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/catalogs/redhat-operator-index/upstream-packages.json", nil)
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
			Expect(rec.Body.String()).To(ContainSubstring("failed to load upstream catalog"))
		})
	})

	// -- handleSignatures --

	Describe("handleSignatures", func() {
		It("returns signature ConfigMap YAML when signatures exist", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet(), newSignatureConfigMap())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/signature-configmaps.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			body := rec.Body.String()
			Expect(body).To(ContainSubstring("ConfigMap"))
			Expect(body).To(ContainSubstring("openshift-config-managed"))
			Expect(body).To(ContainSubstring("sha256-aabbccddee11-1"))
		})

		It("returns placeholder when no signatures ConfigMap exists", func() {
			mux := setupServer(newMirrorTarget(), newReadyImageSet())

			req := httptest.NewRequest(http.MethodGet,
				"/resources/my-imageset/signature-configmaps.yaml", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/yaml"))
			Expect(rec.Body.String()).To(ContainSubstring("No release signatures available"))
		})
	})
})
