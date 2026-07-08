package mirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// buildHelmChartArchive builds a minimal, valid Helm chart .tgz named
// "mychart" version "1.0.0" with a single Deployment template referencing
// the given image.
func buildHelmChartArchive(image string) []byte {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	files := map[string]string{
		"mychart/Chart.yaml": "apiVersion: v2\nname: mychart\nversion: 1.0.0\n",
		"mychart/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
        - name: myapp
          image: ` + image + `
`,
	}
	for path, content := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path, Size: int64(len(content)), Mode: 0644})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gzw.Close()
	return buf.Bytes()
}

var _ = Describe("Collector", func() {
	var (
		col *Collector
		mc  *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient(nil, "")
		col = NewCollector(mc)
	})

	Context("CollectHelm", func() {
		var srv *httptest.Server

		AfterEach(func() {
			if srv != nil {
				srv.Close()
			}
		})

		It("resolves a chart from a repository and preserves the image reference as the destination suffix", func() {
			archive := buildHelmChartArchive("quay.io/example/myapp:1.2.3")
			mux := http.NewServeMux()
			mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
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
			srv = httptest.NewServer(mux)

			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Helm: mirrorv1alpha1.Helm{
						Repositories: []mirrorv1alpha1.Repository{
							{
								Name: "myrepo",
								URL:  srv.URL,
								Charts: []mirrorv1alpha1.Chart{
									{Name: "mychart", Version: "1.0.0"},
								},
							},
						},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "internal.registry.io"},
			}

			results, err := col.CollectHelm(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Source).To(Equal("quay.io/example/myapp:1.2.3"))
			Expect(results[0].Destination).To(Equal("internal.registry.io/quay.io/example/myapp:1.2.3"))
		})

		It("skips a repository that fails to resolve without returning an error", func() {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}))

			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Helm: mirrorv1alpha1.Helm{
						Repositories: []mirrorv1alpha1.Repository{
							{Name: "broken", URL: srv.URL, Charts: []mirrorv1alpha1.Chart{{Name: "mychart"}}},
						},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "internal.registry.io"}}

			results, err := col.CollectHelm(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})

		It("returns empty when no helm repositories are configured", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{}
			target := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "internal.registry.io"}}
			results, err := col.CollectHelm(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})
	})

	Context("CollectTargetImages", func() {
		It("should collect additional images correctly", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/custom/img:v1"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "internal.registry.io",
				},
			}
			meta := &state.Metadata{MirroredImages: make(map[string]string)}

			results, err := col.CollectTargetImages(context.TODO(), spec, target, meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Source).To(Equal("quay.io/custom/img:v1"))
			Expect(results[0].Destination).To(Equal("internal.registry.io/quay.io/custom/img:v1"))
		})
	})

	Context("BlockImages", func() {
		It("removes an additional image matching a blocked name", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/custom/img:v1"},
						{Name: "quay.io/custom/other:v1"},
					},
					BlockedImages: []mirrorv1alpha1.BlockedImage{
						{Name: "custom/img"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "internal.registry.io"},
			}

			results, err := col.CollectTargetImages(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Source).To(Equal("quay.io/custom/other:v1"))
		})

		It("matches regardless of the blocked name's own registry, blocking every tag", func() {
			images := []TargetImage{
				{Source: "quay.io/foo/bar:v1", Destination: "mirror.io/foo/bar:v1"},
				{Source: "quay.io/foo/bar:v2", Destination: "mirror.io/foo/bar:v2"},
				{Source: "quay.io/foo/baz:v1", Destination: "mirror.io/foo/baz:v1"},
			}
			blocked := []mirrorv1alpha1.BlockedImage{{Name: "otherregistry.io/foo/bar"}}
			filtered := BlockImages(images, blocked)
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Source).To(Equal("quay.io/foo/baz:v1"))
		})

		It("narrows to a single tag when the blocked name specifies one", func() {
			images := []TargetImage{
				{Source: "quay.io/foo/bar:v1", Destination: "mirror.io/foo/bar:v1"},
				{Source: "quay.io/foo/bar:v2", Destination: "mirror.io/foo/bar:v2"},
			}
			blocked := []mirrorv1alpha1.BlockedImage{{Name: "foo/bar:v1"}}
			filtered := BlockImages(images, blocked)
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Source).To(Equal("quay.io/foo/bar:v2"))
		})

		It("narrows to a single digest when the blocked name specifies one", func() {
			images := []TargetImage{
				{Source: "nvcr.io/nvidia/driver@sha256:aaa", Destination: "mirror.io/nvidia/driver:sha256-aaa"},
				{Source: "nvcr.io/nvidia/driver@sha256:bbb", Destination: "mirror.io/nvidia/driver:sha256-bbb"},
			}
			blocked := []mirrorv1alpha1.BlockedImage{{Name: "nvidia/driver@sha256:aaa"}}
			filtered := BlockImages(images, blocked)
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Source).To(Equal("nvcr.io/nvidia/driver@sha256:bbb"))
		})

		It("does not block a differently-tagged image when the blocked name specifies a tag", func() {
			images := []TargetImage{{Source: "quay.io/foo/bar:v2", Destination: "mirror.io/foo/bar:v2"}}
			blocked := []mirrorv1alpha1.BlockedImage{{Name: "foo/bar:v1"}}
			Expect(BlockImages(images, blocked)).To(HaveLen(1))
		})

		It("is a no-op when no images are blocked", func() {
			images := []TargetImage{{Source: "quay.io/foo/bar:v1", Destination: "d"}}
			Expect(BlockImages(images, nil)).To(Equal(images))
		})
	})

	Context("Type conversion", func() {
		It("should convert v1alpha1 to v2alpha1 correctly", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{
							Catalog: "redhat-operators",
							IncludeConfig: mirrorv1alpha1.IncludeConfig{
								Packages: []mirrorv1alpha1.IncludePackage{
									{Name: "test-pkg"},
								},
							},
						},
					},
				},
			}
			_, _ = col.CollectTargetImages(context.TODO(), spec, &mirrorv1alpha1.MirrorTarget{}, nil)
		})
	})

	Context("CollectReleasesForChannel destination tagging", func() {
		var (
			target *mirrorv1alpha1.MirrorTarget
			spec   *mirrorv1alpha1.ImageSetSpec
		)

		BeforeEach(func() {
			target = &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{
					Registry: "internal.registry.io",
				},
			}
			spec = &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Platform: mirrorv1alpha1.Platform{
						Architectures: []string{"amd64"},
					},
				},
			}
		})

		It("should tag each payload with its own version when multiple nodes are resolved (minVersion-only)", func() {
			// Simulate what ResolveReleaseNodes returns for minVersion=4.21.9 only.
			// Each node must get its own version tag, not a shared ":latest".
			payloadNodes := []release.Node{
				{Version: "4.21.9", Image: "quay.io/openshift-release-dev/ocp-release@sha256:aaa"},
				{Version: "4.21.10", Image: "quay.io/openshift-release-dev/ocp-release@sha256:bbb"},
				{Version: "4.21.11", Image: "quay.io/openshift-release-dev/ocp-release@sha256:ccc"},
			}
			rel := mirrorv1alpha1.ReleaseChannel{Name: "stable-4.21", MinVersion: "4.21.9"}

			// ExtractComponentImages will fail (nil registry client) — that's OK,
			// the payload images are already appended before component extraction.
			results, err := col.CollectReleasesForChannel(context.TODO(), spec, target, rel, payloadNodes)
			Expect(err).NotTo(HaveOccurred())

			// Collect destinations that contain a payload version tag.
			var payloadDests []string
			for _, r := range results {
				if strings.Contains(r.Destination, ":4.21.") {
					payloadDests = append(payloadDests, r.Destination)
				}
			}

			// Every resolved version must have its own distinct destination
			// in the oc-mirror v2 release-images repository layout.
			Expect(payloadDests).To(ConsistOf(
				"internal.registry.io/openshift/release-images:4.21.9-x86_64",
				"internal.registry.io/openshift/release-images:4.21.10-x86_64",
				"internal.registry.io/openshift/release-images:4.21.11-x86_64",
			))
		})

		It("should tag a single pinned payload with its exact version (maxVersion-only)", func() {
			payloadNodes := []release.Node{
				{Version: "4.21.9", Image: "quay.io/openshift-release-dev/ocp-release@sha256:aaa"},
			}
			rel := mirrorv1alpha1.ReleaseChannel{Name: "stable-4.21", MaxVersion: "4.21.9"}

			results, err := col.CollectReleasesForChannel(context.TODO(), spec, target, rel, payloadNodes)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())
			Expect(results[0].Destination).To(Equal("internal.registry.io/openshift/release-images:4.21.9-x86_64"))
		})

		It("should tag the latest payload with its actual version when no constraints are set", func() {
			// Simulate the single latest-node returned by ResolveReleaseNodes with no constraints.
			payloadNodes := []release.Node{
				{Version: "4.21.11", Image: "quay.io/openshift-release-dev/ocp-release@sha256:ccc"},
			}
			rel := mirrorv1alpha1.ReleaseChannel{Name: "stable-4.21"}

			results, err := col.CollectReleasesForChannel(context.TODO(), spec, target, rel, payloadNodes)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())
			Expect(results[0].Destination).To(Equal("internal.registry.io/openshift/release-images:4.21.11-x86_64"))
		})
	})

	Describe("ImageNamePath", func() {
		DescribeTable("extracts the repository path",
			func(input, expected string) {
				Expect(ImageNamePath(input)).To(Equal(expected))
			},
			Entry("digest-only",
				"quay.io/foo/bar@sha256:abc123",
				"foo/bar"),
			Entry("tag-only",
				"quay.io/foo/bar:v1.2",
				"foo/bar"),
			Entry("tag and digest",
				"quay.io/foo/bar:v1.2@sha256:abc123",
				"foo/bar"),
			Entry("registry with port, tag only",
				"localhost:5001/org/bundle:v1.2.3",
				"org/bundle"),
			Entry("registry with port, digest only",
				"localhost:5001/org/bundle@sha256:abc123",
				"org/bundle"),
			Entry("registry with port, tag and digest",
				"localhost:5001/org/bundle:v1.2.3@sha256:abc123",
				"org/bundle"),
			Entry("no registry prefix",
				"my-image:v1.0",
				"my-image"),
		)
	})

	Describe("ComponentDestination", func() {
		const reg = "mirror.example.com"

		DescribeTable("builds deterministic destination from digest",
			func(src, expected string) {
				Expect(ComponentDestination(reg, src)).To(Equal(expected))
			},
			Entry("digest-only",
				"quay.io/org/bundle@sha256:abc123",
				"mirror.example.com/org/bundle:sha256-abc123"),
			Entry("tag and digest",
				"quay.io/org/bundle:v1.2.3@sha256:abc123",
				"mirror.example.com/org/bundle:sha256-abc123"),
			Entry("registry with port, tag and digest",
				"localhost:5001/org/bundle:v1.2.3@sha256:abc123",
				"mirror.example.com/org/bundle:sha256-abc123"),
		)
	})
})
