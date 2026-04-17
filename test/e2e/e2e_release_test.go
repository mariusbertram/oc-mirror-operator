/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"sort"

	"github.com/blang/semver/v4"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
)

// Cincinnati integration tests use the real public API at api.openshift.com.
// They require outbound HTTPS access from the test environment.
var _ = Describe("Cincinnati Release Resolver", Label("release", "integration"), func() {
	var (
		resolver *release.ReleaseResolver
		ctx      context.Context
	)

	BeforeEach(func() {
		// No registry client is needed for release resolution (HTTP only).
		resolver = release.New(nil)
		ctx = context.Background()
	})

	Context("FetchGraph", func() {
		It("should return a non-empty graph for stable-4.15", func() {
			graph, err := resolver.FetchGraph(ctx, "stable-4.15", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred(), "Cincinnati API should be reachable")
			Expect(graph).NotTo(BeNil())
			Expect(graph.Nodes).NotTo(BeEmpty(), "stable-4.15 must contain at least one node")

			By("verifying every node has a version string and a digest payload")
			for _, node := range graph.Nodes {
				Expect(node.Version).To(MatchRegexp(`^\d+\.\d+\.\d+`),
					"node version must look like a semver")
				Expect(node.Image).To(ContainSubstring("quay.io/openshift-release-dev/ocp-release@sha256:"),
					"payload must be a digest reference in quay.io")
			}
		})
	})

	Context("full channel fetch", func() {
		It("should return all release payload images from stable-4.15", func() {
			images, err := resolver.ResolveRelease(ctx, "stable-4.15", "", "", []string{"amd64"}, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(images).NotTo(BeEmpty())

			By("verifying each image is a digest reference in quay.io")
			for _, img := range images {
				Expect(img).To(ContainSubstring("quay.io/openshift-release-dev/ocp-release@sha256:"),
					"unexpected image format: %s", img)
			}
		})

		It("should return a different (smaller) set for stable-4.14", func() {
			images415, err415 := resolver.ResolveRelease(ctx, "stable-4.15", "", "", []string{"amd64"}, true, false)
			images414, err414 := resolver.ResolveRelease(ctx, "stable-4.14", "", "", []string{"amd64"}, true, false)
			Expect(err415).NotTo(HaveOccurred())
			Expect(err414).NotTo(HaveOccurred())
			// Different channels → different payloads; sets must not be identical
			Expect(images415).NotTo(Equal(images414), "4.14 and 4.15 channels must differ")
		})
	})

	Context("maxVersion pin", func() {
		It("should return exactly one image for a specific release version", func() {
			// Fetch graph first to get a real version that exists in the channel.
			graph, err := resolver.FetchGraph(ctx, "stable-4.14", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(graph.Nodes).NotTo(BeEmpty())

			// Pick the first node as our pinned target.
			target := graph.Nodes[0]

			pinned, err := resolver.ResolveRelease(ctx, "stable-4.14", "", target.Version, []string{"amd64"}, false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(pinned).To(HaveLen(1), "a pinned maxVersion must resolve to exactly one payload")
			Expect(pinned[0]).To(Equal(target.Image), "resolved payload must match the graph node")
		})
	})

	Context("version range filter (minVersion + maxVersion)", func() {
		It("should return fewer images than the full channel", func() {
			graph, err := resolver.FetchGraph(ctx, "stable-4.14", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(graph.Nodes)).To(BeNumerically(">=", 4),
				"need at least 4 nodes to test a narrower range")

			// Sort a copy of the nodes by semver to get a stable low/high pair.
			type versionedNode struct {
				version string
				image   string
				sv      semver.Version
			}
			var sorted []versionedNode
			for _, n := range graph.Nodes {
				sv, parseErr := semver.ParseTolerant(n.Version)
				if parseErr != nil {
					continue
				}
				sorted = append(sorted, versionedNode{n.Version, n.Image, sv})
			}
			sort.Slice(sorted, func(i, j int) bool {
				return sorted[i].sv.LT(sorted[j].sv)
			})
			Expect(len(sorted)).To(BeNumerically(">=", 3))

			minVer := sorted[0].version
			maxVer := sorted[2].version

			rangeImages, err := resolver.ResolveRelease(ctx, "stable-4.14", minVer, maxVer, []string{"amd64"}, false, false)
			fullImages, errFull := resolver.ResolveRelease(ctx, "stable-4.14", "", "", []string{"amd64"}, true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(errFull).NotTo(HaveOccurred())

			Expect(rangeImages).NotTo(BeEmpty(), "range [%s, %s] must contain at least one image", minVer, maxVer)
			Expect(len(rangeImages)).To(BeNumerically("<", len(fullImages)),
				"a narrow version range must produce fewer images than the full channel")

			By("verifying all ranged images are valid digest references")
			for _, img := range rangeImages {
				Expect(img).To(ContainSubstring("@sha256:"), "must be a digest ref: %s", img)
			}
		})
	})

	Context("ExtractComponentImages (release payload layer parsing)", func() {
		It("should extract ~190 component images from a real release payload", func() {
			// We need a real MirrorClient for registry access.
			mc := mirrorclient.NewMirrorClient(nil, "")
			resolver := release.New(mc)

			// Fetch one real payload image from the Cincinnati graph to use as test input.
			graph, err := resolver.FetchGraph(ctx, "stable-4.15", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())
			Expect(graph.Nodes).NotTo(BeEmpty())

			// Pick the first node as our test payload.
			payloadImage := graph.Nodes[0].Image

			By(fmt.Sprintf("extracting component images from payload %s", payloadImage))
			components, err := resolver.ExtractComponentImages(ctx, payloadImage, "amd64")
			Expect(err).NotTo(HaveOccurred(),
				"ExtractComponentImages must succeed against a real release payload")

			By("verifying the component image count is approximately 189")
			Expect(len(components)).To(BeNumerically(">=", 150),
				"expected ~189 component images, got %d", len(components))
			Expect(len(components)).To(BeNumerically("<=", 250),
				"unexpectedly many component images: %d", len(components))

			By("verifying all component images are digest references in quay.io/openshift-release-dev")
			for _, img := range components {
				Expect(img).To(ContainSubstring("@sha256:"),
					"component image must be a digest reference: %s", img)
				Expect(img).To(ContainSubstring("quay.io/openshift-release-dev"),
					"component image must be from quay.io/openshift-release-dev: %s", img)
			}

			By("verifying no duplicate image references")
			seen := make(map[string]struct{}, len(components))
			for _, img := range components {
				_, dup := seen[img]
				Expect(dup).To(BeFalse(), "duplicate component image: %s", img)
				seen[img] = struct{}{}
			}
		})
	})

	Context("shortestPath via BFS", func() {
		It("should return a path that starts and ends at the requested nodes", func() {
			graph, err := resolver.FetchGraph(ctx, "stable-4.14", []string{"amd64"})
			Expect(err).NotTo(HaveOccurred())

			// Need at least two connected nodes.
			if len(graph.Edges) == 0 {
				Skip("stable-4.14 graph has no edges; cannot test BFS path")
			}

			// Pick a real edge from the graph so we know a path exists.
			edge := graph.Edges[0]
			Expect(edge).To(HaveLen(2))
			fromVer := graph.Nodes[edge[0]].Version
			toVer := graph.Nodes[edge[1]].Version

			path, err := resolver.ResolveRelease(ctx, "stable-4.14", fromVer, toVer, []string{"amd64"}, false, true)
			Expect(err).NotTo(HaveOccurred(), "BFS must find a path for a known graph edge")
			Expect(path).NotTo(BeEmpty())
			Expect(path[0]).To(Equal(graph.Nodes[edge[0]].Image), "path must start with the from-node payload")
			Expect(path[len(path)-1]).To(Equal(graph.Nodes[edge[1]].Image), "path must end with the to-node payload")
		})
	})
})
