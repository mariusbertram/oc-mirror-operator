package mirror

import (
	"context"
	"time"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
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
		It("uses version as tag", func() {
			dest := releasePayloadDestination("mirror.io", "4.15.3", "quay.io/openshift/ocp-release@sha256:abc")
			Expect(dest).To(Equal("mirror.io/openshift/ocp-release:4.15.3"))
		})

		It("falls back to latest when version is empty", func() {
			dest := releasePayloadDestination("mirror.io", "", "quay.io/openshift/ocp-release@sha256:abc")
			Expect(dest).To(Equal("mirror.io/openshift/ocp-release:latest"))
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
	})

	// ── WorkerPool extended ───────────────────────────────────────────
	Describe("WorkerPool", func() {
		It("Submit returns error when pool context cancelled and channel full", func() {
			ctx, cancel := context.WithCancel(context.Background())
			mc := mirrorclient.NewMirrorClient(nil, "")
			pool := NewWorkerPool(ctx, mc, 0) // 0 workers → tasks channel is never drained

			// Fill the tasks buffer (capacity 100)
			for i := 0; i < 100; i++ {
				_ = pool.Submit(context.Background(), Task{Source: "fill", Destination: "fill"})
			}
			cancel()

			err := pool.Submit(context.Background(), Task{Source: "a", Destination: "b"})
			Expect(err).To(HaveOccurred())
		})

		It("Submit returns error when caller context cancelled and channel full", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			pool := NewWorkerPool(context.Background(), mc, 0) // 0 workers
			defer pool.Stop()

			// Fill the tasks buffer (capacity 100)
			for i := 0; i < 100; i++ {
				_ = pool.Submit(context.Background(), Task{Source: "fill", Destination: "fill"})
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := pool.Submit(ctx, Task{Source: "a", Destination: "b"})
			Expect(err).To(HaveOccurred())
		})

		It("Stop closes tasks and results channels", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			pool := NewWorkerPool(context.Background(), mc, 2)
			pool.Stop()

			// results channel should be closed
			_, open := <-pool.Results()
			Expect(open).To(BeFalse())
		})

		It("workers exit when context cancelled", func() {
			ctx, cancel := context.WithCancel(context.Background())
			mc := mirrorclient.NewMirrorClient(nil, "")
			pool := NewWorkerPool(ctx, mc, 2)
			cancel()
			// Give workers time to exit
			time.Sleep(50 * time.Millisecond)
			pool.Stop()
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
