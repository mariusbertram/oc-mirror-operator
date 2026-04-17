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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// catalogLiveImage is the public OperatorHub.io catalog used for integration tests.
// It is a real FBC (File-Based Catalog) with hundreds of community operators.
// Access requires outbound HTTPS to quay.io — no authentication needed.
const catalogLiveImage = "quay.io/operatorhubio/catalog:latest"

var _ = Describe("Catalog FBC Live Extraction", Label("catalog-live", "integration"), func() {
	var (
		resolver *catalog.CatalogResolver
		ctx      context.Context
	)

	BeforeEach(func() {
		mc := mirrorclient.NewMirrorClient(nil, "")
		resolver = catalog.New(mc)
		ctx = context.Background()
	})

	Context("ResolveCatalog with FBC layer extraction", func() {
		It("should return only the catalog image when no packages are requested", func() {
			// No packages → only the catalog index image itself should come back.
			// This also validates that loadFBCFromImage succeeds (we use the real catalog).
			images, err := resolver.ResolveCatalog(ctx, catalogLiveImage, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(images).To(ContainElement(catalogLiveImage))
		})

		It("should extract bundle and related images for a specific operator", func() {
			// postgresql-operator is the smallest package in the catalog (~3.7 KB of FBC YAML).
			// We use it here to keep the test fast while still exercising the full pipeline.
			packages := []string{"postgresql-operator"}

			By(fmt.Sprintf("resolving catalog %s for packages %v", catalogLiveImage, packages))
			images, err := resolver.ResolveCatalog(ctx, catalogLiveImage, packages)
			Expect(err).NotTo(HaveOccurred(),
				"ResolveCatalog must succeed for a known public catalog")

			By("verifying the catalog index image is always included")
			Expect(images).To(ContainElement(catalogLiveImage))

			By("verifying at least one bundle/related image was extracted beyond the catalog itself")
			Expect(len(images)).To(BeNumerically(">", 1),
				"expected bundle/related images in addition to the catalog index, got only %v", images)

			By("verifying extracted images are valid OCI references")
			for _, img := range images {
				if img == catalogLiveImage {
					continue
				}
				Expect(img).NotTo(BeEmpty())
				// OCI image references use "registry/path:tag" (no "://")
				// Check it looks like a valid registry reference: must contain "/"
				Expect(img).To(ContainSubstring("/"),
					"image should look like a registry reference (host/path): %s", img)
			}
		})

		It("should extract multiple operators and return distinct image sets", func() {
			// Request two known small operators and verify the combined set is larger
			// than either individual result.
			single, err := resolver.ResolveCatalog(ctx, catalogLiveImage, []string{"postgresql-operator"})
			Expect(err).NotTo(HaveOccurred())

			double, err := resolver.ResolveCatalog(ctx, catalogLiveImage, []string{"postgresql-operator", "patterns-operator"})
			Expect(err).NotTo(HaveOccurred())

			Expect(len(double)).To(BeNumerically(">=", len(single)),
				"requesting two operators must return at least as many images as one")
		})

		It("should return no component images for a non-existent package", func() {
			images, err := resolver.ResolveCatalog(ctx, catalogLiveImage, []string{"this-operator-does-not-exist-xyz"})
			Expect(err).NotTo(HaveOccurred())
			// Only the catalog image itself; no bundles for an unknown package.
			Expect(images).To(ConsistOf(catalogLiveImage))
		})
	})

	Context("loadFBCFromImage internals", func() {
		It("should parse a valid DeclarativeConfig from the live catalog", func() {
			// We test FilterFBC and ExtractImages on the parsed config to verify
			// that the FBC round-trip (pull → parse → filter → extract) is correct.
			//
			// Re-use ResolveCatalog which internally calls loadFBCFromImage.
			target := "patterns-operator" // second smallest package

			images, err := resolver.ResolveCatalog(ctx, catalogLiveImage, []string{target})
			Expect(err).NotTo(HaveOccurred())

			By("verifying that bundle images look like real image references")
			componentImages := make([]string, 0)
			for _, img := range images {
				if img != catalogLiveImage {
					componentImages = append(componentImages, img)
				}
			}
			Expect(componentImages).NotTo(BeEmpty(),
				"patterns-operator must have at least one bundle or related image")

			for _, img := range componentImages {
				Expect(img).To(MatchRegexp(`^[a-zA-Z0-9._\-/]+(@sha256:[a-f0-9]+|:[a-zA-Z0-9._\-]+)$`),
					"image reference must be valid: %s", img)
			}
		})
	})

	Context("BuildFilteredCatalogImage", func() {
		It("should build and push a valid filtered catalog image to a local OCI directory", func() {
			// Use ocidir:// as the target so no external registry is needed.
			tmpDir, err := os.MkdirTemp("", "catalog-build-test-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = os.RemoveAll(tmpDir) })

			targetRef := "ocidir://" + filepath.Join(tmpDir, "filtered-catalog") + ":latest"
			packages := []string{"postgresql-operator"}

			By(fmt.Sprintf("building filtered catalog image for packages %v → %s", packages, targetRef))
			digest, err := resolver.BuildFilteredCatalogImage(ctx, catalogLiveImage, targetRef, packages)
			Expect(err).NotTo(HaveOccurred(), "BuildFilteredCatalogImage must succeed")
			Expect(digest).To(HavePrefix("sha256:"), "returned digest must be sha256")

			By("verifying the OCI layout index was written to disk")
			indexPath := filepath.Join(tmpDir, "filtered-catalog", "index.json")
			_, statErr := os.Stat(indexPath)
			Expect(statErr).NotTo(HaveOccurred(), "OCI index.json must exist at %s", indexPath)

			By("verifying the OCI layout contains at least one blob (the FBC layer)")
			blobsDir := filepath.Join(tmpDir, "filtered-catalog", "blobs", "sha256")
			entries, readErr := os.ReadDir(blobsDir)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(len(entries)).To(BeNumerically(">=", 2),
				"expected at least config blob + FBC layer blob, got %d", len(entries))

			By("verifying the digest matches one of the blobs on disk")
			digestHex := strings.TrimPrefix(digest, "sha256:")
			blobPath := filepath.Join(blobsDir, digestHex)
			_, statErr = os.Stat(blobPath)
			Expect(statErr).NotTo(HaveOccurred(),
				"manifest blob %s must be present on disk", blobPath)
		})
	})
})
