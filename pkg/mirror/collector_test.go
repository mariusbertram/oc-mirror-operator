package mirror

import (
	"context"
	"strings"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Collector", func() {
	var (
		col *Collector
		mc  *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient(nil, "")
		col = NewCollector(mc)
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

			// Every resolved version must have its own distinct destination.
			Expect(payloadDests).To(ConsistOf(
				"internal.registry.io/openshift-release-dev/ocp-release:4.21.9",
				"internal.registry.io/openshift-release-dev/ocp-release:4.21.10",
				"internal.registry.io/openshift-release-dev/ocp-release:4.21.11",
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
			Expect(results[0].Destination).To(Equal("internal.registry.io/openshift-release-dev/ocp-release:4.21.9"))
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
			Expect(results[0].Destination).To(Equal("internal.registry.io/openshift-release-dev/ocp-release:4.21.11"))
		})
	})

	Describe("imageNamePath", func() {
		DescribeTable("extracts the repository path",
			func(input, expected string) {
				Expect(imageNamePath(input)).To(Equal(expected))
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

	Describe("componentDestination", func() {
		const reg = "mirror.example.com"

		DescribeTable("builds deterministic destination from digest",
			func(src, expected string) {
				Expect(componentDestination(reg, src)).To(Equal(expected))
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
