package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	ociV1 "github.com/regclient/regclient/types/oci/v1"
)

var _ = Describe("Coverage Tests", func() {

	// ── releasePayloadDestination ─────────────────────────────────────
	Describe("releasePayloadDestination", func() {
		It("uses the oc-mirror v2 release-images repo with version-arch tag", func() {
			dest := releasePayloadDestination("mirror.io", releaseTagFor("4.15.3", "amd64"))
			Expect(dest).To(Equal("mirror.io/openshift/release-images:4.15.3-x86_64"))
		})

		It("falls back to latest when version is empty", func() {
			dest := releasePayloadDestination("mirror.io", releaseTagFor("", "amd64"))
			Expect(dest).To(Equal("mirror.io/openshift/release-images:latest"))
		})
	})

	// ── releaseComponentDestination ───────────────────────────────────
	Describe("releaseComponentDestination", func() {
		It("places named components in openshift/release with version-arch-name tag", func() {
			dest := releaseComponentDestination("mirror.io", releaseTagFor("4.15.3", "amd64"), "etcd",
				"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc")
			Expect(dest).To(Equal("mirror.io/openshift/release:4.15.3-x86_64-etcd"))
		})

		It("falls back to the digest-tagged source path for unnamed components", func() {
			dest := releaseComponentDestination("mirror.io", releaseTagFor("4.15.3", "amd64"), "",
				"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc")
			Expect(dest).To(Equal("mirror.io/openshift-release-dev/ocp-v4.0-art-dev:sha256-abc"))
		})
	})

	// ── releaseArchName ───────────────────────────────────────────────
	Describe("releaseArchName", func() {
		It("maps GOARCH names to release tag arch names", func() {
			Expect(releaseArchName("amd64")).To(Equal("x86_64"))
			Expect(releaseArchName("arm64")).To(Equal("aarch64"))
			Expect(releaseArchName("ppc64le")).To(Equal("ppc64le"))
			Expect(releaseArchName("s390x")).To(Equal("s390x"))
			Expect(releaseArchName("multi")).To(Equal("multi"))
		})
	})

	// ── toTargetImage ─────────────────────────────────────────────────
	Describe("toTargetImage", func() {
		var col *Collector

		BeforeEach(func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col = NewCollector(mc)
		})

		It("returns Pending when meta is nil", func() {
			ti := col.toTargetImage("src", "dest", nil)
			Expect(ti.State).To(Equal("Pending"))
			Expect(ti.Source).To(Equal("src"))
			Expect(ti.Destination).To(Equal("dest"))
		})

		It("returns Pending when meta has no entry for dest", func() {
			meta := &state.Metadata{MirroredImages: map[string]string{"other": "sha256:x"}}
			ti := col.toTargetImage("src", "dest", meta)
			Expect(ti.State).To(Equal("Pending"))
		})

		It("returns Mirrored when meta has entry for dest", func() {
			meta := &state.Metadata{MirroredImages: map[string]string{"dest": "sha256:x"}}
			ti := col.toTargetImage("src", "dest", meta)
			Expect(ti.State).To(Equal("Mirrored"))
		})
	})

	// ── CollectAdditional ─────────────────────────────────────────────
	Describe("CollectAdditional", func() {
		var col *Collector

		BeforeEach(func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col = NewCollector(mc)
		})

		It("handles empty additional images", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectAdditional(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})

		It("uses TargetRepo when specified", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/img:v1", TargetRepo: "custom/path:v1"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectAdditional(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Destination).To(Equal("mirror.io/custom/path:v1"))
		})

		It("applies TargetTag over the source tag when TargetRepo is unset", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/img:v1", TargetTag: "custom-tag"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectAdditional(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Destination).To(Equal("mirror.io/quay.io/img:custom-tag"))
		})

		It("applies TargetTag over an inline tag in TargetRepo", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/img:v1", TargetRepo: "custom/path:v1", TargetTag: "v2"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectAdditional(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Destination).To(Equal("mirror.io/custom/path:v2"))
		})

		It("applies TargetTag over a digest-referenced source", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/img@sha256:abc123", TargetTag: "custom-tag"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectAdditional(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Destination).To(Equal("mirror.io/quay.io/img:custom-tag"))
		})

		It("marks Mirrored when meta has the dest", func() {
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/img:v1"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			meta := &state.Metadata{MirroredImages: map[string]string{
				"mirror.io/quay.io/img:v1": "sha256:abc",
			}}
			results, err := col.CollectAdditional(context.TODO(), spec, target, meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(results[0].State).To(Equal("Mirrored"))
		})
	})

	// ── CollectOperators ──────────────────────────────────────────────
	Describe("CollectOperators", func() {
		It("returns empty when no operators defined", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			spec := &mirrorv1alpha1.ImageSetSpec{}
			target := &mirrorv1alpha1.MirrorTarget{}
			results, err := col.CollectOperators(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})

		It("appends images from a successfully resolved catalog", func() {
			catalogImage := pushOperatorCatalog(GinkgoTB(), "pkg-multi")
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{
							Catalog: catalogImage,
							IncludeConfig: mirrorv1alpha1.IncludeConfig{
								Packages: []mirrorv1alpha1.IncludePackage{{Name: "pkg-multi"}},
							},
						},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"}}

			results, err := col.CollectOperators(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
		})
	})

	// ── CollectReleases ───────────────────────────────────────────────
	Describe("CollectReleases", func() {
		It("returns empty when no channels defined", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			spec := &mirrorv1alpha1.ImageSetSpec{}
			target := &mirrorv1alpha1.MirrorTarget{}
			results, err := col.CollectReleases(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})

		It("appends images from a successfully resolved channel", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(release.Graph{
					Nodes: []release.Node{{Version: "4.15.1", Image: "quay.io/openshift-release-dev/ocp-release@sha256:ccc"}},
				})
			}))
			defer server.Close()
			origURL := release.OcpUpdateURL
			release.OcpUpdateURL = server.URL
			defer func() { release.OcpUpdateURL = origURL }()

			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Platform: mirrorv1alpha1.Platform{
						Channels: []mirrorv1alpha1.ReleaseChannel{{Name: "stable-4.15"}},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"}}

			results, err := col.CollectReleases(context.TODO(), spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())
			Expect(results[0].Destination).To(Equal("mirror.io/openshift/release-images:4.15.1-x86_64"))
		})
	})

	// ── PlanMirrorOrder ───────────────────────────────────────────────
	Describe("PlanMirrorOrder", func() {
		It("returns empty slices for empty input", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			src, dst := PlanMirrorOrder(context.Background(), mc, nil, nil)
			Expect(src).To(BeNil())
			Expect(dst).To(BeNil())
		})

		It("returns single item unchanged", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			src, dst := PlanMirrorOrder(context.Background(), mc, []string{"src1"}, []string{"dst1"})
			Expect(src).To(Equal([]string{"src1"}))
			Expect(dst).To(Equal([]string{"dst1"}))
		})

		It("orders multiple items with failed manifest fetches (empty blobs)", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			// All manifest fetches fail → empty blobs → greedy order still completes
			src, dst := PlanMirrorOrder(context.Background(), mc,
				[]string{"localhost:1/a@sha256:a", "localhost:1/b@sha256:b", "localhost:1/c@sha256:c"},
				[]string{"dst-a", "dst-b", "dst-c"})
			Expect(src).To(HaveLen(3))
			Expect(dst).To(HaveLen(3))
		})

		It("orders real images by actual shared-blob overlap via manifest inspection", func() {
			t := GinkgoTB()
			// img0 and img1 share layer "A"; img1 and img2 share layer "B"; all
			// three exercise a real ManifestGet + blob-digest round trip, driving
			// both the first-pick (frequency) and subsequent-pick (uploaded-count)
			// scoring branches with non-empty blob sets.
			img0 := pushSingleManifestImage(t, [][]byte{[]byte("layer-A"), []byte("layer-C")})
			img1 := pushSingleManifestImage(t, [][]byte{[]byte("layer-A"), []byte("layer-B")})
			img2 := pushSingleManifestImage(t, [][]byte{[]byte("layer-B"), []byte("layer-D")})

			mc := mirrorclient.NewMirrorClient(nil, "")
			src, dst := PlanMirrorOrder(context.Background(), mc,
				[]string{img0, img1, img2}, []string{"dst-0", "dst-1", "dst-2"})

			Expect(src).To(ConsistOf(img0, img1, img2))
			Expect(dst).To(ConsistOf("dst-0", "dst-1", "dst-2"))
			// img1 shares a blob with both other images, so it should be scheduled first.
			Expect(src[0]).To(Equal(img1))
		})
	})

	// ── ComponentDestination without digest ───────────────────────────
	Describe("ComponentDestination", func() {
		It("returns path without tag when no digest present", func() {
			dest := ComponentDestination("mirror.io", "quay.io/org/img:v1")
			Expect(dest).To(Equal("mirror.io/org/img"))
		})
	})

	// ── collectBlobs ─────────────────────────────────────────────────
	Describe("collectBlobs", func() {
		It("extracts config and layer digests from a manifest", func() {
			ociM := ociV1.Manifest{
				Versioned: ociV1.ManifestSchemaVersion,
				MediaType: mediatype.OCI1Manifest,
				Config: descriptor.Descriptor{
					MediaType: mediatype.OCI1ImageConfig,
					Digest:    godigest.FromString("config-data"),
					Size:      11,
				},
				Layers: []descriptor.Descriptor{
					{
						MediaType: mediatype.OCI1Layer,
						Digest:    godigest.FromString("layer1-data"),
						Size:      11,
					},
					{
						MediaType: mediatype.OCI1Layer,
						Digest:    godigest.FromString("layer2-data"),
						Size:      11,
					},
				},
			}
			m, err := manifest.New(manifest.WithOrig(ociM))
			Expect(err).NotTo(HaveOccurred())

			blobs := map[string]struct{}{}
			collectBlobs(m, blobs)

			Expect(blobs).To(HaveLen(3)) // 1 config + 2 layers
			Expect(blobs).To(HaveKey(godigest.FromString("config-data").String()))
			Expect(blobs).To(HaveKey(godigest.FromString("layer1-data").String()))
			Expect(blobs).To(HaveKey(godigest.FromString("layer2-data").String()))
		})
	})

	// ── CollectReleases with channels ─────────────────────────────────
	Describe("CollectReleases with unreachable channel", func() {
		It("continues past failed channels and returns empty", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // force immediate failure

			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Platform: mirrorv1alpha1.Platform{
						Channels: []mirrorv1alpha1.ReleaseChannel{
							{Name: "stable-4.15"},
						},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectReleases(ctx, spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())
		})
	})

	// ── CollectTargetImages operators error ────────────────────────────
	Describe("CollectTargetImages with operator that fails", func() {
		It("continues past operator resolution failures", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{Catalog: "localhost:1/catalog:v1"},
					},
					AdditionalImages: []mirrorv1alpha1.AdditionalImage{
						{Name: "quay.io/extra:v1"},
					},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			results, err := col.CollectTargetImages(ctx, spec, target, nil)
			Expect(err).NotTo(HaveOccurred())
			// Operators fail gracefully; additional images still collected
			Expect(results).To(HaveLen(1))
			Expect(results[0].Source).To(Equal("quay.io/extra:v1"))
		})
	})

	// ── extractBlobDigests error paths ─────────────────────────────────
	Describe("extractBlobDigests", func() {
		It("returns error for invalid ref", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			_, err := extractBlobDigests(context.Background(), mc, ":::invalid")
			Expect(err).To(HaveOccurred())
		})

		It("returns error when the manifest cannot be fetched", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			dir := GinkgoT().TempDir()
			_, err := extractBlobDigests(context.Background(), mc, "ocidir://"+dir+":missing-tag")
			Expect(err).To(HaveOccurred())
		})

		It("collects config and layer digests from a single-platform manifest", func() {
			t := GinkgoTB()
			image := pushSingleManifestImage(t, [][]byte{[]byte("layer-data")})
			mc := mirrorclient.NewMirrorClient(nil, "")
			blobs, err := extractBlobDigests(context.Background(), mc, image)
			Expect(err).NotTo(HaveOccurred())
			// 1 config blob + 1 layer blob.
			Expect(blobs).To(HaveLen(2))
		})

		It("resolves each platform manifest in a manifest list, skipping ones that fail to fetch", func() {
			t := GinkgoTB()
			dir := t.TempDir()
			image := pushManifestListImage(t, dir,
				map[string][][]byte{"amd64": {[]byte("amd64-layer")}, "arm64": {[]byte("arm64-layer")}},
				map[string]bool{"arm64": true},
			)
			mc := mirrorclient.NewMirrorClient(nil, "")
			blobs, err := extractBlobDigests(context.Background(), mc, image)
			Expect(err).NotTo(HaveOccurred())
			// amd64 child: config + layer resolved successfully, plus both
			// child manifest digests themselves recorded from the index.
			Expect(len(blobs)).To(BeNumerically(">=", 3))
		})
	})

	// ── CollectOperatorEntry error path ────────────────────────────────
	Describe("CollectOperatorEntry", func() {
		It("returns error when catalog resolution fails", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			_, err := col.CollectOperatorEntry(ctx, mirrorv1alpha1.Operator{
				Catalog: "localhost:1/catalog:v1",
			}, target)
			Expect(err).To(HaveOccurred())
		})

		It("resolves a real catalog, building destinations and BundleRef labels", func() {
			catalogImage := pushOperatorCatalog(GinkgoTB(), "pkg-op")

			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			op := mirrorv1alpha1.Operator{
				Catalog: catalogImage,
				IncludeConfig: mirrorv1alpha1.IncludeConfig{
					Packages: []mirrorv1alpha1.IncludePackage{{Name: "pkg-op"}},
				},
			}

			results, err := col.CollectOperatorEntry(context.Background(), op, target)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			bySource := make(map[string]TargetImage, len(results))
			for _, r := range results {
				bySource[r.Source] = r
			}

			bundle, ok := bySource["registry.example.com/pkg-op-bundle@sha256:0000000000000000000000000000000000000000000000000000000000000000"]
			Expect(ok).To(BeTrue())
			Expect(bundle.Destination).To(Equal("mirror.io/pkg-op-bundle:sha256-0000000000000000000000000000000000000000000000000000000000000000"))
			Expect(bundle.BundleRef).To(Equal("pkg-op.v1.0.0"))
			Expect(bundle.State).To(Equal("Pending"))

			related, ok := bySource["registry.example.com/pkg-op-extra@sha256:1111111111111111111111111111111111111111111111111111111111111111"]
			Expect(ok).To(BeTrue())
			Expect(related.BundleRef).To(Equal("pkg-op.v1.0.0"))
		})
	})

	// ── CollectReleasesForChannel error path ──────────────────────────
	Describe("CollectReleasesForChannel", func() {
		It("returns error when resolve fails and no payload nodes provided", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			spec := &mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Platform: mirrorv1alpha1.Platform{Architectures: []string{"amd64"}},
				},
			}
			target := &mirrorv1alpha1.MirrorTarget{
				Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "mirror.io"},
			}
			_, err := col.CollectReleasesForChannel(ctx, spec, target, mirrorv1alpha1.ReleaseChannel{Name: "stable-4.15"}, nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ResolveReleasePayloadNodes", func() {
		It("defaults to amd64 when no archs specified", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			col := NewCollector(mc)
			// Use a cancelled context so it fails fast
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := col.ResolveReleasePayloadNodes(ctx, mirrorv1alpha1.ReleaseChannel{
				Name: "stable-4.15",
			}, nil)
			Expect(err).To(HaveOccurred())
		})
	})
})
