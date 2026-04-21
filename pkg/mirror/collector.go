package mirror

import (
	"context"
	"fmt"
	"strings"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
)

// TargetImage defines a source and destination for mirroring
type TargetImage struct {
	Source      string
	Destination string
	State       string
}

// Collector gathers the list of target images from an ImageSet configuration
type Collector struct {
	client          *mirrorclient.MirrorClient
	releaseResolver *release.ReleaseResolver
	catalogResolver *catalog.CatalogResolver
}

func NewCollector(client *mirrorclient.MirrorClient) *Collector {
	return &Collector{
		client:          client,
		releaseResolver: release.New(client),
		catalogResolver: catalog.New(client),
	}
}

// CollectTargetImages parses the ImageSet configuration and returns the list of images to mirror
func (c *Collector) CollectTargetImages(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, meta *state.Metadata) ([]TargetImage, error) {
	var results []TargetImage

	// 1. Collect Releases
	for _, rel := range spec.Mirror.Platform.Channels {
		arch := spec.Mirror.Platform.Architectures
		if len(arch) == 0 {
			arch = []string{"amd64"}
		}

		// Resolve the effective max version for use in destination paths.
		// When the user hasn't pinned a version we look it up dynamically.
		effectiveMaxVersion := rel.MaxVersion
		if effectiveMaxVersion == "" && !rel.Full && rel.MinVersion == "" {
			resolved, resolveErr := c.releaseResolver.ResolveLatestVersion(ctx, rel.Name, arch)
			if resolveErr != nil {
				fmt.Printf("Warning: failed to resolve latest version for channel %s: %v\n", rel.Name, resolveErr)
			} else {
				effectiveMaxVersion = resolved
			}
		}

		payloadImages, err := c.releaseResolver.ResolveRelease(ctx, rel.Name, rel.MinVersion, rel.MaxVersion, arch, rel.Full, rel.ShortestPath)
		if err != nil {
			fmt.Printf("Warning: failed to resolve release %s/%s: %v\n", rel.Name, rel.MaxVersion, err)
			continue
		}

		// For each release payload image, extract the ~190 component images.
		for _, payloadImg := range payloadImages {
			// Always include the payload image itself (needed for the release update graph).
			// Tag it with the release version so it is addressable by version number.
			dest := releasePayloadDestination(target.Spec.Registry, effectiveMaxVersion, payloadImg)
			results = append(results, c.toTargetImage(payloadImg, dest, meta))

			// Extract component images from the payload's image-references layer.
			componentImages, extractErr := c.releaseResolver.ExtractComponentImages(ctx, payloadImg, arch[0])
			if extractErr != nil {
				fmt.Printf("Warning: failed to extract component images from %s: %v\n", payloadImg, extractErr)
				continue
			}
			for _, compImg := range componentImages {
				compDest := componentDestination(target.Spec.Registry, compImg)
				results = append(results, c.toTargetImage(compImg, compDest, meta))
			}

			// Extract KubeVirt container disk images if requested.
			if spec.Mirror.Platform.KubeVirtContainer {
				kvImages, kvErr := c.releaseResolver.ExtractKubeVirtImages(ctx, payloadImg, arch)
				if kvErr != nil {
					fmt.Printf("Warning: failed to extract KubeVirt images from %s: %v\n", payloadImg, kvErr)
				} else {
					for _, kvImg := range kvImages {
						kvDest := componentDestination(target.Spec.Registry, kvImg)
						results = append(results, c.toTargetImage(kvImg, kvDest, meta))
						fmt.Printf("KubeVirt image added: %s\n", kvImg)
					}
				}
			}
		}
	}

	// 2. Collect Operators (bundle and related images)
	// The filtered catalog image is built and pushed by a separate CatalogBuildJob;
	// here we only collect the bundle/related images that need to be mirrored.
	for _, op := range spec.Mirror.Operators {
		pkgs := []string{}
		for _, p := range op.Packages {
			pkgs = append(pkgs, p.Name)
		}

		images, err := c.catalogResolver.ResolveCatalog(ctx, op.Catalog, pkgs)
		if err != nil {
			fmt.Printf("Warning: failed to resolve catalog %s: %v\n", op.Catalog, err)
			continue
		}
		for _, img := range images {
			dest := componentDestination(target.Spec.Registry, img)
			results = append(results, c.toTargetImage(img, dest, meta))
		}
	}

	// 3. Collect AdditionalImages
	for _, img := range spec.Mirror.AdditionalImages {
		dest := fmt.Sprintf("%s/%s", target.Spec.Registry, img.Name)
		if img.TargetRepo != "" {
			dest = fmt.Sprintf("%s/%s", target.Spec.Registry, img.TargetRepo)
		}
		results = append(results, c.toTargetImage(img.Name, dest, meta))
	}

	return results, nil
}

func (c *Collector) toTargetImage(src, dest string, meta *state.Metadata) TargetImage {
	s := "Pending"
	if meta != nil && meta.MirroredImages[dest] != "" {
		s = "Mirrored"
	}
	return TargetImage{
		Source:      src,
		Destination: dest,
		State:       s,
	}
}

// imageNamePath strips the registry host from an image reference and returns only the
// repository path (e.g. "quay.io/foo/bar@sha256:…" → "foo/bar").
func imageNamePath(img string) string {
	imgNoTag := strings.Split(img, ":")[0]
	imgNoDigest := strings.Split(imgNoTag, "@")[0]
	parts := strings.SplitN(imgNoDigest, "/", 2)
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		return parts[1]
	}
	return imgNoDigest
}

// releasePayloadDestination builds the destination for a release payload image.
// The release version is used as the tag so the image is addressable by version.
//   e.g. quay.io/openshift-release-dev/ocp-release@sha256:abc → registry/openshift-release-dev/ocp-release:4.21.9
func releasePayloadDestination(registry, releaseVersion, img string) string {
	tag := releaseVersion
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("%s/%s:%s", registry, imageNamePath(img), tag)
}

// componentDestination builds the destination for a component image (release or
// operator bundle). The source digest is encoded as the tag so each distinct
// image gets a unique, deterministic destination.
//   e.g. quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:184844… →
//        registry/openshift-release-dev/ocp-v4.0-art-dev:sha256-184844…
func componentDestination(registry, img string) string {
	namePath := imageNamePath(img)
	// Derive a stable tag from the digest so each component gets a unique destination.
	if idx := strings.Index(img, "@sha256:"); idx >= 0 {
		tag := "sha256-" + img[idx+8:] // "sha256-{hex}"
		return fmt.Sprintf("%s/%s:%s", registry, namePath, tag)
	}
	return fmt.Sprintf("%s/%s", registry, namePath)
}

