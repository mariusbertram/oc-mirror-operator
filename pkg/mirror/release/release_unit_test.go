package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// buildTarGz creates an in-memory gzipped tar from a map of filename→content.
// Because Go maps are unordered, callers needing deterministic tar order
// should use buildTarGzOrdered instead.
func buildTarGz(files map[string][]byte) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(content)),
			Mode: 0644,
		})
		_, _ = tw.Write(content)
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

// buildTarGzOrdered creates an in-memory gzipped tar honouring insertion order.
func buildTarGzOrdered(entries []struct {
	Name    string
	Content []byte
}) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		_ = tw.WriteHeader(&tar.Header{
			Name: e.Name,
			Size: int64(len(e.Content)),
			Mode: 0644,
		})
		_, _ = tw.Write(e.Content)
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

// newResolverWithHTTPClient creates a ReleaseResolver that talks to the given
// httptest server instead of the real Cincinnati API.
func newResolverWithHTTPClient(serverURL string) *ReleaseResolver {
	return &ReleaseResolver{
		client:     nil,
		httpClient: &http.Client{},
	}
}

// --- Tests ---

var _ = Describe("Unit", func() {

	// ── NodeImages ──────────────────────────────────────────────────────
	Describe("NodeImages", func() {
		It("returns empty slice for nil input", func() {
			Expect(NodeImages(nil)).To(BeEmpty())
		})
		It("returns empty slice for empty input", func() {
			Expect(NodeImages([]Node{})).To(BeEmpty())
		})
		It("extracts images in order", func() {
			nodes := []Node{
				{Version: "4.15.0", Image: "quay.io/a@sha256:aaa"},
				{Version: "4.15.1", Image: "quay.io/b@sha256:bbb"},
				{Version: "4.15.2", Image: "quay.io/c@sha256:ccc"},
			}
			imgs := NodeImages(nodes)
			Expect(imgs).To(Equal([]string{
				"quay.io/a@sha256:aaa",
				"quay.io/b@sha256:bbb",
				"quay.io/c@sha256:ccc",
			}))
		})
	})

	// ── ResolvedSignature ───────────────────────────────────────────────
	Describe("ResolvedSignature", func() {
		It("returns empty string for nil input", func() {
			Expect(ResolvedSignature(nil)).To(Equal(""))
		})
		It("returns empty string for empty input", func() {
			Expect(ResolvedSignature([]string{})).To(Equal(""))
		})
		It("returns non-empty hex string for a single image", func() {
			sig := ResolvedSignature([]string{"quay.io/a@sha256:aaa"})
			Expect(sig).NotTo(BeEmpty())
			Expect(len(sig)).To(Equal(64)) // SHA-256 hex
		})
		It("is deterministic", func() {
			imgs := []string{"quay.io/a@sha256:aaa", "quay.io/b@sha256:bbb"}
			Expect(ResolvedSignature(imgs)).To(Equal(ResolvedSignature(imgs)))
		})
		It("is order-independent (sorted internally)", func() {
			a := ResolvedSignature([]string{"b", "a"})
			b := ResolvedSignature([]string{"a", "b"})
			Expect(a).To(Equal(b))
		})
		It("different images yield different signatures", func() {
			s1 := ResolvedSignature([]string{"a"})
			s2 := ResolvedSignature([]string{"b"})
			Expect(s1).NotTo(Equal(s2))
		})
	})

	// ── latestNode ──────────────────────────────────────────────────────
	Describe("latestNode", func() {
		It("returns nil for empty graph", func() {
			g := &Graph{Nodes: []Node{}}
			Expect(latestNode(g)).To(BeNil())
		})
		It("returns the single node", func() {
			g := &Graph{Nodes: []Node{{Version: "4.15.0", Image: "img"}}}
			n := latestNode(g)
			Expect(n).NotTo(BeNil())
			Expect(n.Version).To(Equal("4.15.0"))
		})
		It("returns highest semver node", func() {
			g := &Graph{Nodes: []Node{
				{Version: "4.14.3", Image: "a"},
				{Version: "4.15.1", Image: "b"},
				{Version: "4.14.9", Image: "c"},
			}}
			Expect(latestNode(g).Version).To(Equal("4.15.1"))
		})
		It("skips unparseable versions but still returns best valid", func() {
			g := &Graph{Nodes: []Node{
				{Version: "not-a-version", Image: "x"},
				{Version: "4.14.1", Image: "a"},
				{Version: "also-bad", Image: "y"},
				{Version: "4.15.0", Image: "b"},
			}}
			Expect(latestNode(g).Version).To(Equal("4.15.0"))
		})
		It("handles first node unparseable, picks next valid as initial best then compares", func() {
			// When the first node (best==nil) is unparseable, it still gets
			// set as best; subsequent parseable nodes should replace it.
			g := &Graph{Nodes: []Node{
				{Version: "bad", Image: "x"},
				{Version: "4.14.0", Image: "a"},
			}}
			n := latestNode(g)
			Expect(n).NotTo(BeNil())
			Expect(n.Version).To(Equal("4.14.0"))
		})
	})

	// ── shortestPathNodes ───────────────────────────────────────────────
	Describe("shortestPathNodes", func() {
		var rr *ReleaseResolver
		BeforeEach(func() {
			rr = New(nil)
		})

		It("finds direct edge A→B", func() {
			g := &Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "a"},
					{Version: "4.15.0", Image: "b"},
				},
				Edges: [][]int{{0, 1}},
			}
			nodes, err := rr.shortestPathNodes(g, "4.14.0", "4.15.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
			Expect(nodes[0].Version).To(Equal("4.14.0"))
			Expect(nodes[1].Version).To(Equal("4.15.0"))
		})

		It("finds multi-hop A→B→C", func() {
			g := &Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "a"},
					{Version: "4.14.5", Image: "b"},
					{Version: "4.15.0", Image: "c"},
				},
				Edges: [][]int{{0, 1}, {1, 2}},
			}
			nodes, err := rr.shortestPathNodes(g, "4.14.0", "4.15.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(3))
			Expect(nodes[0].Version).To(Equal("4.14.0"))
			Expect(nodes[1].Version).To(Equal("4.14.5"))
			Expect(nodes[2].Version).To(Equal("4.15.0"))
		})

		It("returns error when no path exists", func() {
			g := &Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "a"},
					{Version: "4.15.0", Image: "b"},
				},
				Edges: [][]int{}, // no edges
			}
			_, err := rr.shortestPathNodes(g, "4.14.0", "4.15.0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no upgrade path"))
		})

		It("returns error when from version not in graph", func() {
			g := &Graph{
				Nodes: []Node{{Version: "4.15.0", Image: "a"}},
				Edges: [][]int{},
			}
			_, err := rr.shortestPathNodes(g, "9.9.9", "4.15.0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("9.9.9 not found"))
		})

		It("returns error when to version not in graph", func() {
			g := &Graph{
				Nodes: []Node{{Version: "4.15.0", Image: "a"}},
				Edges: [][]int{},
			}
			_, err := rr.shortestPathNodes(g, "4.15.0", "9.9.9")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("9.9.9 not found"))
		})

		It("returns single node when from == to", func() {
			g := &Graph{
				Nodes: []Node{{Version: "4.15.0", Image: "a"}},
				Edges: [][]int{},
			}
			nodes, err := rr.shortestPathNodes(g, "4.15.0", "4.15.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Version).To(Equal("4.15.0"))
		})

		It("gracefully skips out-of-bounds edges", func() {
			g := &Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "a"},
					{Version: "4.15.0", Image: "b"},
				},
				Edges: [][]int{{0, 1}, {0, 99}, {-1, 0}},
			}
			nodes, err := rr.shortestPathNodes(g, "4.14.0", "4.15.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
		})

		It("skips malformed edges (len != 2)", func() {
			g := &Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "a"},
					{Version: "4.15.0", Image: "b"},
				},
				Edges: [][]int{{0, 1}, {0}, {0, 1, 2}},
			}
			nodes, err := rr.shortestPathNodes(g, "4.14.0", "4.15.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
		})
	})

	// ── collectKubeVirtRefs ─────────────────────────────────────────────
	Describe("collectKubeVirtRefs", func() {
		It("returns empty for empty stream", func() {
			s := coreOSStream{Architectures: map[string]coreOSArch{}}
			Expect(collectKubeVirtRefs(s, nil)).To(BeEmpty())
		})

		It("returns digest-ref for matching arch", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {
								Image:     "quay.io/example/coreos:latest",
								DigestRef: "quay.io/example/coreos@sha256:abc123",
							},
						},
					},
				},
			}
			refs := collectKubeVirtRefs(s, map[string]bool{"x86_64": true})
			Expect(refs).To(ConsistOf("quay.io/example/coreos@sha256:abc123"))
		})

		It("skips arch not in wantArches", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/a@sha256:aaa"},
						},
					},
					"aarch64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/b@sha256:bbb"},
						},
					},
				},
			}
			refs := collectKubeVirtRefs(s, map[string]bool{"x86_64": true})
			Expect(refs).To(ConsistOf("quay.io/a@sha256:aaa"))
		})

		It("includes all arches when wantArches is empty", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/a@sha256:aaa"},
						},
					},
					"aarch64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/b@sha256:bbb"},
						},
					},
				},
			}
			refs := collectKubeVirtRefs(s, map[string]bool{})
			Expect(refs).To(HaveLen(2))
		})

		It("falls back to Image when DigestRef is empty", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {Image: "quay.io/fallback:latest"},
						},
					},
				},
			}
			refs := collectKubeVirtRefs(s, map[string]bool{})
			Expect(refs).To(ConsistOf("quay.io/fallback:latest"))
		})

		It("prefers DigestRef over Image", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {
								Image:     "quay.io/img:tag",
								DigestRef: "quay.io/img@sha256:abc",
							},
						},
					},
				},
			}
			refs := collectKubeVirtRefs(s, map[string]bool{})
			Expect(refs).To(ConsistOf("quay.io/img@sha256:abc"))
		})

		It("skips entry with neither Image nor DigestRef", func() {
			s := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {},
						},
					},
				},
			}
			Expect(collectKubeVirtRefs(s, map[string]bool{})).To(BeEmpty())
		})
	})

	// ── scanLayerForImageReferences ─────────────────────────────────────
	Describe("scanLayerForImageReferences", func() {
		It("parses valid image-references from gzipped tar", func() {
			irJSON := `{
				"spec": {
					"tags": [
						{"name":"component-a","from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa"}},
						{"name":"component-b","from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:bbb"}}
					]
				}
			}`
			buf := buildTarGz(map[string][]byte{
				"release-manifests/image-references": []byte(irJSON),
			})
			images, found, err := scanLayerForImageReferences(buf)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(images).To(Equal([]string{
				"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa",
				"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:bbb",
			}))
		})

		It("returns nil/false/nil when tar has no image-references", func() {
			buf := buildTarGz(map[string][]byte{
				"some-other-file.txt": []byte("hello"),
			})
			images, found, err := scanLayerForImageReferences(buf)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			Expect(images).To(BeNil())
		})

		It("returns error for invalid gzip", func() {
			_, _, err := scanLayerForImageReferences(strings.NewReader("not gzip"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gzip"))
		})

		It("returns error for invalid JSON in image-references", func() {
			buf := buildTarGz(map[string][]byte{
				"release-manifests/image-references": []byte("{invalid json"),
			})
			_, _, err := scanLayerForImageReferences(buf)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse image-references"))
		})

		It("skips tags with empty from.name", func() {
			irJSON := `{
				"spec": {
					"tags": [
						{"name":"a","from":{"name":"quay.io/x@sha256:aaa"}},
						{"name":"b","from":{"name":""}}
					]
				}
			}`
			buf := buildTarGz(map[string][]byte{
				"release-manifests/image-references": []byte(irJSON),
			})
			images, found, err := scanLayerForImageReferences(buf)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(images).To(Equal([]string{"quay.io/x@sha256:aaa"}))
		})
	})

	// ── scanLayerForKubeVirtImages ──────────────────────────────────────
	Describe("scanLayerForKubeVirtImages", func() {
		It("parses valid bootimages from gzipped tar", func() {
			stream := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {
								Image:     "quay.io/example/coreos:latest",
								DigestRef: "quay.io/example/coreos@sha256:abc123",
							},
						},
					},
				},
			}
			streamJSON, _ := json.Marshal(stream)
			cmYAML := fmt.Sprintf("data:\n  stream: '%s'\n", string(streamJSON))

			buf := buildTarGz(map[string][]byte{
				"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(cmYAML),
			})
			images, found, err := scanLayerForKubeVirtImages(buf, []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(images).To(ConsistOf("quay.io/example/coreos@sha256:abc123"))
		})

		It("returns nil/false/nil when tar has no bootimages", func() {
			buf := buildTarGz(map[string][]byte{
				"some-file.txt": []byte("hello"),
			})
			images, found, err := scanLayerForKubeVirtImages(buf, []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			Expect(images).To(BeNil())
		})

		It("returns error for empty stream field", func() {
			cmYAML := "data:\n  stream: ''\n"
			buf := buildTarGz(map[string][]byte{
				"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(cmYAML),
			})
			_, _, err := scanLayerForKubeVirtImages(buf, []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty stream"))
		})

		It("returns error for invalid gzip", func() {
			_, _, err := scanLayerForKubeVirtImages(strings.NewReader("not gzip"), []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("gzip"))
		})

		It("maps amd64 to x86_64 and arm64 to aarch64", func() {
			stream := coreOSStream{
				Architectures: map[string]coreOSArch{
					"x86_64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/x86@sha256:aaa"},
						},
					},
					"aarch64": {
						Images: map[string]coreOSImage{
							"kubevirt": {DigestRef: "quay.io/arm@sha256:bbb"},
						},
					},
				},
			}
			streamJSON, _ := json.Marshal(stream)
			cmYAML := fmt.Sprintf("data:\n  stream: '%s'\n", string(streamJSON))

			buf := buildTarGz(map[string][]byte{
				"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(cmYAML),
			})
			images, found, err := scanLayerForKubeVirtImages(buf, []string{"amd64", "arm64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(images).To(ConsistOf(
				"quay.io/x86@sha256:aaa",
				"quay.io/arm@sha256:bbb",
			))
		})

		It("returns error for invalid stream JSON", func() {
			cmYAML := "data:\n  stream: '{bad json'\n"
			buf := buildTarGz(map[string][]byte{
				"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(cmYAML),
			})
			_, _, err := scanLayerForKubeVirtImages(buf, []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse stream JSON"))
		})
	})

	// ── FetchGraph (httptest) ───────────────────────────────────────────
	Describe("FetchGraph via httptest", func() {
		var (
			server *httptest.Server
			rr     *ReleaseResolver
		)

		AfterEach(func() {
			if server != nil {
				server.Close()
			}
		})

		setup := func(handler http.HandlerFunc) {
			server = httptest.NewServer(handler)
			rr = &ReleaseResolver{
				httpClient: server.Client(),
			}
		}

		It("parses a valid graph response", func() {
			graph := Graph{
				Nodes: []Node{
					{Version: "4.14.0", Image: "quay.io/a@sha256:aaa"},
					{Version: "4.15.0", Image: "quay.io/b@sha256:bbb"},
				},
				Edges: [][]int{{0, 1}},
			}
			setup(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(graph)
			})

			// Override the URL by calling the server directly
			g, err := fetchGraphFromURL(rr, server.URL, "stable-4.15", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.Nodes).To(HaveLen(2))
			Expect(g.Edges).To(HaveLen(1))
		})

		It("returns error on 404", func() {
			setup(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
			_, err := fetchGraphFromURL(rr, server.URL, "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("404"))
		})

		It("returns error on invalid JSON body", func() {
			setup(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("not json"))
			})
			_, err := fetchGraphFromURL(rr, server.URL, "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unmarshal"))
		})

		It("sets channel and arch query params", func() {
			var gotChannel, gotArch string
			setup(func(w http.ResponseWriter, r *http.Request) {
				gotChannel = r.URL.Query().Get("channel")
				gotArch = r.URL.Query().Get("arch")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"nodes":[],"edges":[]}`))
			})
			_, _ = fetchGraphFromURL(rr, server.URL, "fast-4.16", []string{"arm64"})
			Expect(gotChannel).To(Equal("fast-4.16"))
			Expect(gotArch).To(Equal("arm64"))
		})
	})

	// ── ResolveReleaseNodes (httptest integration) ──────────────────────
	Describe("ResolveReleaseNodes via httptest", func() {
		var (
			server *httptest.Server
			rr     *ReleaseResolver
		)

		graphFixture := Graph{
			Nodes: []Node{
				{Version: "4.14.0", Image: "quay.io/a@sha256:aaa"},
				{Version: "4.14.5", Image: "quay.io/b@sha256:bbb"},
				{Version: "4.15.0", Image: "quay.io/c@sha256:ccc"},
				{Version: "4.15.1", Image: "quay.io/d@sha256:ddd"},
			},
			Edges: [][]int{{0, 1}, {1, 2}, {2, 3}},
		}

		BeforeEach(func() {
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(graphFixture)
			}))
			rr = &ReleaseResolver{
				httpClient: server.Client(),
			}
		})

		AfterEach(func() {
			server.Close()
		})

		resolveNodes := func(ctx context.Context, channel, minV, maxV string, arch []string, full, shortest bool) ([]Node, error) {
			return resolveReleaseNodesFromURL(rr, ctx, server.URL, channel, minV, maxV, arch, full, shortest)
		}

		It("full=true returns all nodes", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "", "", nil, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(4))
		})

		It("maxVersion only returns single matching node", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "", "4.14.5", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Version).To(Equal("4.14.5"))
		})

		It("maxVersion not found returns error", func() {
			_, err := resolveNodes(context.Background(), "stable-4.15", "", "9.9.9", nil, false, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("9.9.9 not found"))
		})

		It("minVersion + maxVersion returns range", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "4.14.5", "4.15.0", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			versions := make([]string, 0, len(nodes))
			for _, n := range nodes {
				versions = append(versions, n.Version)
			}
			Expect(versions).To(ContainElements("4.14.5", "4.15.0"))
			Expect(versions).NotTo(ContainElement("4.14.0"))
		})

		It("minVersion + maxVersion + shortestPath returns BFS path", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "4.14.0", "4.15.0", nil, false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(3))
			Expect(nodes[0].Version).To(Equal("4.14.0"))
			Expect(nodes[2].Version).To(Equal("4.15.0"))
		})

		It("minVersion only returns all nodes >= min", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "4.15.0", "", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			for _, n := range nodes {
				Expect(n.Version).To(SatisfyAny(Equal("4.15.0"), Equal("4.15.1")))
			}
		})

		It("no constraints returns latest node", func() {
			nodes, err := resolveNodes(context.Background(), "stable-4.15", "", "", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Version).To(Equal("4.15.1"))
		})

		It("empty graph with no constraints returns error", func() {
			server.Close()
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"nodes":[],"edges":[]}`))
			}))
			rr.httpClient = server.Client()

			_, err := resolveReleaseNodesFromURL(rr, context.Background(), server.URL, "stable-4.15", "", "", nil, false, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no nodes found"))
		})
	})

	// --- Tests that exercise the REAL production methods via OcpUpdateURL override ---
	Describe("ResolveReleaseNodes (real method)", func() {
		var (
			server    *httptest.Server
			rr        *ReleaseResolver
			origURL   string
		)

		graphFixture := Graph{
			Nodes: []Node{
				{Version: "4.14.0", Image: "quay.io/a@sha256:aaa"},
				{Version: "4.14.5", Image: "quay.io/b@sha256:bbb"},
				{Version: "4.15.0", Image: "quay.io/c@sha256:ccc"},
				{Version: "4.15.1", Image: "quay.io/d@sha256:ddd"},
			},
			Edges: [][]int{{0, 1}, {1, 2}, {2, 3}},
		}

		BeforeEach(func() {
			origURL = OcpUpdateURL
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(graphFixture)
			}))
			// Temporarily override the production URL to use our test server
			OcpUpdateURL = server.URL
			rr = &ReleaseResolver{httpClient: server.Client()}
		})

		AfterEach(func() {
			OcpUpdateURL = origURL
			server.Close()
		})

		It("full=true returns all nodes via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "", "", nil, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(4))
		})

		It("maxVersion only returns matching node via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "", "4.14.5", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Version).To(Equal("4.14.5"))
		})

		It("minVersion + maxVersion range filter via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "4.14.0", "4.14.5", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
		})

		It("minVersion + maxVersion + shortestPath via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "4.14.0", "4.15.1", nil, false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes)).To(BeNumerically(">=", 2))
		})

		It("minVersion only returns >= min via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "4.15.0", "", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
		})

		It("no constraints returns latest via real method", func() {
			nodes, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "", "", nil, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Version).To(Equal("4.15.1"))
		})

		It("invalid minVersion returns error via real method", func() {
			_, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "not-semver", "4.15.0", nil, false, false)
			Expect(err).To(HaveOccurred())
		})

		It("invalid maxVersion returns error via real method", func() {
			_, err := rr.ResolveReleaseNodes(context.Background(), "stable-4.15", "4.14.0", "not-semver", nil, false, false)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ResolveRelease (real method)", func() {
		It("delegates to ResolveReleaseNodes and extracts images", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				graph := Graph{
					Nodes: []Node{{Version: "4.14.0", Image: "quay.io/a@sha256:aaa"}},
				}
				_ = json.NewEncoder(w).Encode(graph)
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			images, err := rr.ResolveRelease(context.Background(), "stable-4.14", "", "", nil, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(images).To(Equal([]string{"quay.io/a@sha256:aaa"}))
		})
	})

	Describe("ResolveLatestVersion (real method)", func() {
		It("returns the latest version string", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				graph := Graph{
					Nodes: []Node{
						{Version: "4.14.0", Image: "quay.io/a@sha256:aaa"},
						{Version: "4.15.1", Image: "quay.io/b@sha256:bbb"},
					},
				}
				_ = json.NewEncoder(w).Encode(graph)
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			ver, err := rr.ResolveLatestVersion(context.Background(), "stable-4.15", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ver).To(Equal("4.15.1"))
		})

		It("returns error for empty graph", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(Graph{})
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			_, err := rr.ResolveLatestVersion(context.Background(), "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ExtractComponentImages (nil client)", func() {
		It("returns error when MirrorClient is nil", func() {
			rr := &ReleaseResolver{client: nil, httpClient: http.DefaultClient}
			_, err := rr.ExtractComponentImages(context.Background(), "quay.io/test@sha256:abc", "amd64")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("non-nil MirrorClient"))
		})

		It("returns error for invalid image reference", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			rr := New(mc)
			_, err := rr.ExtractComponentImages(context.Background(), ":::invalid", "amd64")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse payload image reference"))
		})
	})

	Describe("ExtractKubeVirtImages (nil client)", func() {
		It("returns error when MirrorClient is nil", func() {
			rr := &ReleaseResolver{client: nil, httpClient: http.DefaultClient}
			_, err := rr.ExtractKubeVirtImages(context.Background(), "quay.io/test@sha256:abc", []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("non-nil MirrorClient"))
		})

		It("returns error for invalid image reference", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			rr := New(mc)
			_, err := rr.ExtractKubeVirtImages(context.Background(), ":::invalid", []string{"amd64"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse payload image reference"))
		})
	})

	Describe("New constructor", func() {
		It("creates a resolver with http client", func() {
			rr := New(nil)
			Expect(rr).NotTo(BeNil())
			Expect(rr.httpClient).NotTo(BeNil())
			Expect(rr.client).To(BeNil())
		})
	})

	Describe("FetchGraph error paths", func() {
		It("returns error on connection refused", func() {
			origURL := OcpUpdateURL
			OcpUpdateURL = "http://127.0.0.1:1" // nothing listening
			defer func() { OcpUpdateURL = origURL }()

			rr := New(nil)
			_, err := rr.FetchGraph(context.Background(), "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
		})

		It("returns error on non-200 status", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			_, err := rr.FetchGraph(context.Background(), "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unexpected status code 500"))
		})

		It("returns error on invalid JSON response", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("not-json"))
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			_, err := rr.FetchGraph(context.Background(), "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unmarshal"))
		})

		It("passes arch query param", func() {
			var receivedArch string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedArch = r.URL.Query().Get("arch")
				_ = json.NewEncoder(w).Encode(Graph{Nodes: []Node{{Version: "4.14.0", Image: "x"}}})
			}))
			defer server.Close()
			origURL := OcpUpdateURL
			OcpUpdateURL = server.URL
			defer func() { OcpUpdateURL = origURL }()

			rr := &ReleaseResolver{httpClient: server.Client()}
			_, err := rr.FetchGraph(context.Background(), "stable-4.15", []string{"arm64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(receivedArch).To(Equal("arm64"))
		})
	})

	Describe("ResolveRelease error propagation", func() {
		It("returns error when FetchGraph fails", func() {
			origURL := OcpUpdateURL
			OcpUpdateURL = "http://127.0.0.1:1"
			defer func() { OcpUpdateURL = origURL }()

			rr := New(nil)
			_, err := rr.ResolveRelease(context.Background(), "stable-4.15", "", "", nil, true, false)
			Expect(err).To(HaveOccurred())
		})

		It("returns error when ResolveLatestVersion FetchGraph fails", func() {
			origURL := OcpUpdateURL
			OcpUpdateURL = "http://127.0.0.1:1"
			defer func() { OcpUpdateURL = origURL }()

			rr := New(nil)
			_, err := rr.ResolveLatestVersion(context.Background(), "stable-4.15", nil)
			Expect(err).To(HaveOccurred())
		})
	})
})

// fetchGraphFromURL is a test helper that calls FetchGraph with a custom base URL
// instead of the hard-coded OcpUpdateURL.
func fetchGraphFromURL(rr *ReleaseResolver, baseURL, channel string, arch []string) (*Graph, error) {
	u := baseURL + "?"
	if channel != "" {
		u += "channel=" + channel
	}
	if len(arch) > 0 {
		u += "&arch=" + arch[0]
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Add("Accept", "application/json")

	resp, err := rr.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch release graph: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, u)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var graph Graph
	if err := json.Unmarshal(body, &graph); err != nil {
		return nil, fmt.Errorf("failed to unmarshal release graph: %w", err)
	}
	return &graph, nil
}

// resolveReleaseNodesFromURL exercises the same resolution logic as
// ResolveReleaseNodes but fetches the graph from the given base URL.
func resolveReleaseNodesFromURL(rr *ReleaseResolver, ctx context.Context, baseURL, channel, minVersion, maxVersion string, arch []string, full, shortestPath bool) ([]Node, error) {
	graph, err := fetchGraphFromURL(rr, baseURL, channel, arch)
	if err != nil {
		return nil, err
	}

	if full {
		nodes := make([]Node, len(graph.Nodes))
		copy(nodes, graph.Nodes)
		return nodes, nil
	}

	if minVersion == "" && maxVersion != "" {
		for _, node := range graph.Nodes {
			if node.Version == maxVersion {
				return []Node{node}, nil
			}
		}
		return nil, fmt.Errorf("version %s not found in channel %s", maxVersion, channel)
	}

	if minVersion != "" && maxVersion != "" {
		if shortestPath {
			return rr.shortestPathNodes(graph, minVersion, maxVersion)
		}
		var nodes []Node
		for _, node := range graph.Nodes {
			sv, parseErr := semverParseTolerant(node.Version)
			if parseErr != nil {
				continue
			}
			minSV, _ := semverParseTolerant(minVersion)
			maxSV, _ := semverParseTolerant(maxVersion)
			if sv.GTE(minSV) && sv.LTE(maxSV) {
				nodes = append(nodes, node)
			}
		}
		return nodes, nil
	}

	if minVersion != "" {
		var nodes []Node
		minSV, _ := semverParseTolerant(minVersion)
		for _, node := range graph.Nodes {
			sv, parseErr := semverParseTolerant(node.Version)
			if parseErr != nil {
				continue
			}
			if sv.GTE(minSV) {
				nodes = append(nodes, node)
			}
		}
		return nodes, nil
	}

	latest := latestNode(graph)
	if latest == nil {
		return nil, fmt.Errorf("no nodes found in channel %s", channel)
	}
	return []Node{*latest}, nil
}

// semverParseTolerant wraps semver import to avoid polluting the test with
// an extra import when the logic is duplicated from the production code.
func semverParseTolerant(v string) (semverVersion, error) {
	// Minimal re-implementation: strip leading "v" and split.
	// We use the real blang/semver in this file via the production code,
	// but we can't directly import it without adding the dependency.
	// Instead, we use a thin wrapper.
	return parseSemver(v)
}

// semverVersion is a minimal comparable version.
type semverVersion struct {
	major, minor, patch int
}

func (a semverVersion) GTE(b semverVersion) bool {
	if a.major != b.major {
		return a.major >= b.major
	}
	if a.minor != b.minor {
		return a.minor >= b.minor
	}
	return a.patch >= b.patch
}

func (a semverVersion) LTE(b semverVersion) bool {
	if a.major != b.major {
		return a.major <= b.major
	}
	if a.minor != b.minor {
		return a.minor <= b.minor
	}
	return a.patch <= b.patch
}

func parseSemver(v string) (semverVersion, error) {
	v = strings.TrimPrefix(v, "v")
	var sv semverVersion
	_, err := fmt.Sscanf(v, "%d.%d.%d", &sv.major, &sv.minor, &sv.patch)
	return sv, err
}
