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
	"bytes"
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
)

// buildTestFBC constructs a minimal in-memory FBC with three packages:
//
//	gitops, compliance, acm
//
// Each package has one channel and two bundles that carry distinct related images.
func buildTestFBC() *declcfg.DeclarativeConfig {
	pkg := func(name string) declcfg.Package {
		return declcfg.Package{
			Schema:         "olm.package",
			Name:           name,
			DefaultChannel: "stable",
		}
	}

	ch := func(pkgName, bundleA, bundleB string) declcfg.Channel {
		return declcfg.Channel{
			Schema:  "olm.channel",
			Name:    "stable",
			Package: pkgName,
			Entries: []declcfg.ChannelEntry{
				{Name: bundleA},
				{Name: bundleB, Replaces: bundleA},
			},
		}
	}

	bundle := func(pkgName, name, image string, relImages ...string) declcfg.Bundle {
		ri := make([]declcfg.RelatedImage, 0, len(relImages))
		for _, r := range relImages {
			ri = append(ri, declcfg.RelatedImage{Image: r})
		}
		return declcfg.Bundle{
			Schema:  "olm.bundle",
			Name:    name,
			Package: pkgName,
			Image:   image,
			Properties: []property.Property{
				{Type: "olm.package", Value: []byte(`{"packageName":"` + pkgName + `","version":"0.0.1"}`)},
			},
			RelatedImages: ri,
		}
	}

	return &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{
			pkg("openshift-gitops-operator"),
			pkg("compliance-operator"),
			pkg("advanced-cluster-management"),
		},
		Channels: []declcfg.Channel{
			ch("openshift-gitops-operator", "gitops.v1.0.0", "gitops.v1.1.0"),
			ch("compliance-operator", "compliance.v0.1.0", "compliance.v0.2.0"),
			ch("advanced-cluster-management", "acm.v2.8.0", "acm.v2.9.0"),
		},
		Bundles: []declcfg.Bundle{
			bundle("openshift-gitops-operator", "gitops.v1.0.0",
				"registry.redhat.io/openshift-gitops-1/gitops-rhel8-operator@sha256:aaa001",
				"registry.redhat.io/openshift-gitops-1/gitops-rhel8@sha256:aaa002",
			),
			bundle("openshift-gitops-operator", "gitops.v1.1.0",
				"registry.redhat.io/openshift-gitops-1/gitops-rhel8-operator@sha256:aaa003",
				"registry.redhat.io/openshift-gitops-1/gitops-rhel8@sha256:aaa004",
			),
			bundle("compliance-operator", "compliance.v0.1.0",
				"registry.redhat.io/compliance/compliance-rhel8-operator@sha256:bbb001",
				"registry.redhat.io/compliance/openscap-ocp4@sha256:bbb002",
			),
			bundle("compliance-operator", "compliance.v0.2.0",
				"registry.redhat.io/compliance/compliance-rhel8-operator@sha256:bbb003",
				"registry.redhat.io/compliance/openscap-ocp4@sha256:bbb004",
			),
			bundle("advanced-cluster-management", "acm.v2.8.0",
				"registry.redhat.io/rhacm2/acm-operator-bundle@sha256:ccc001",
				"registry.redhat.io/rhacm2/acm-rhel8-operator@sha256:ccc002",
			),
			bundle("advanced-cluster-management", "acm.v2.9.0",
				"registry.redhat.io/rhacm2/acm-operator-bundle@sha256:ccc003",
				"registry.redhat.io/rhacm2/acm-rhel8-operator@sha256:ccc004",
			),
		},
	}
}

var _ = Describe("Catalog FBC Filter and Index Rebuild", Label("catalog"), func() {
	var resolver *catalog.CatalogResolver

	BeforeEach(func() {
		resolver = catalog.New(nil)
	})

	Context("FilterFBC", func() {
		It("should include only selected packages and exclude others", func(ctx context.Context) {
			cfg := buildTestFBC()
			selected := []mirrorv1alpha1.IncludePackage{
				{Name: "openshift-gitops-operator"},
				{Name: "compliance-operator"},
			}

			filtered, err := resolver.FilterFBC(ctx, cfg, selected)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Packages")
			Expect(filtered.Packages).To(HaveLen(2))
			pkgNames := make([]string, 0, 2)
			for _, p := range filtered.Packages {
				pkgNames = append(pkgNames, p.Name)
			}
			Expect(pkgNames).To(ConsistOf("openshift-gitops-operator", "compliance-operator"))

			By("verifying Channels")
			Expect(filtered.Channels).To(HaveLen(2))
			for _, c := range filtered.Channels {
				Expect(c.Package).NotTo(Equal("advanced-cluster-management"),
					"ACM channel must not appear in filtered FBC")
			}

			By("verifying Bundles")
			Expect(filtered.Bundles).To(HaveLen(4), "2 bundles per selected package = 4 total")
			for _, b := range filtered.Bundles {
				Expect(b.Package).NotTo(Equal("advanced-cluster-management"),
					"ACM bundles must not appear in filtered FBC")
			}
		})

		It("should return the full config when no packages are specified", func(ctx context.Context) {
			cfg := buildTestFBC()

			full, err := resolver.FilterFBC(ctx, cfg, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(full.Packages).To(HaveLen(3))
			Expect(full.Bundles).To(HaveLen(6))
		})
	})

	Context("ExtractImages", func() {
		It("should extract all bundle and related images without duplicates", func() {
			cfg := buildTestFBC()

			images := resolver.ExtractImages(cfg)
			Expect(images).NotTo(BeEmpty())

			// 3 packages × 2 bundles × (1 bundle image + 1 related image) = 12 unique refs
			Expect(images).To(HaveLen(12))

			By("verifying no duplicates")
			seen := make(map[string]struct{}, len(images))
			for _, img := range images {
				_, dup := seen[img]
				Expect(dup).To(BeFalse(), "duplicate image ref: %s", img)
				seen[img] = struct{}{}
			}
		})

		It("should extract only the filtered packages' images after FilterFBC", func(ctx context.Context) {
			cfg := buildTestFBC()
			filtered, err := resolver.FilterFBC(ctx, cfg, []mirrorv1alpha1.IncludePackage{{Name: "compliance-operator"}})
			Expect(err).NotTo(HaveOccurred())

			images := resolver.ExtractImages(filtered)
			Expect(images).NotTo(BeEmpty())

			for _, img := range images {
				Expect(img).To(ContainSubstring("registry.redhat.io/compliance"),
					"only compliance images should remain after filtering")
			}
		})
	})

	Context("Index rebuild (WriteYAML roundtrip)", func() {
		It("should produce valid YAML that can be reloaded as a DeclarativeConfig", func(ctx context.Context) {
			cfg := buildTestFBC()
			selected := []mirrorv1alpha1.IncludePackage{{Name: "openshift-gitops-operator"}}

			filtered, err := resolver.FilterFBC(ctx, cfg, selected)
			Expect(err).NotTo(HaveOccurred())

			By("serialising the filtered FBC to YAML")
			var buf bytes.Buffer
			Expect(declcfg.WriteYAML(*filtered, &buf)).To(Succeed(),
				"WriteYAML must not return an error")
			Expect(buf.Len()).To(BeNumerically(">", 0), "YAML output must not be empty")

			By("verifying the YAML is valid declarative config (roundtrip)")
			reloaded, err := declcfg.LoadReader(strings.NewReader(buf.String()))
			Expect(err).NotTo(HaveOccurred(), "reloading the serialised YAML should succeed")
			Expect(reloaded.Packages).To(HaveLen(1))
			Expect(reloaded.Packages[0].Name).To(Equal("openshift-gitops-operator"))
			Expect(reloaded.Bundles).To(HaveLen(2))
		})

		It("should produce valid JSON index via WriteJSON", func(ctx context.Context) {
			cfg := buildTestFBC()
			selected := []mirrorv1alpha1.IncludePackage{{Name: "compliance-operator"}}

			filtered, err := resolver.FilterFBC(ctx, cfg, selected)
			Expect(err).NotTo(HaveOccurred())

			var buf bytes.Buffer
			Expect(declcfg.WriteJSON(*filtered, &buf)).To(Succeed())

			reloaded, err := declcfg.LoadReader(strings.NewReader(buf.String()))
			Expect(err).NotTo(HaveOccurred())
			Expect(reloaded.Packages).To(HaveLen(1))
			Expect(reloaded.Packages[0].Name).To(Equal("compliance-operator"))
			Expect(reloaded.Channels).To(HaveLen(1))
			Expect(reloaded.Bundles).To(HaveLen(2))
		})
	})

	Context("Empty filter result", func() {
		It("should return empty config when no packages match the filter", func(ctx context.Context) {
			cfg := buildTestFBC()

			empty, err := resolver.FilterFBC(ctx, cfg, []mirrorv1alpha1.IncludePackage{{Name: "non-existent-operator"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(empty.Packages).To(BeEmpty())
			Expect(empty.Channels).To(BeEmpty())
			Expect(empty.Bundles).To(BeEmpty())

			images := resolver.ExtractImages(empty)
			Expect(images).To(BeEmpty())
		})
	})
})
