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
			dest := releaseDestination(target.Spec.Registry, effectiveMaxVersion, payloadImg)
			results = append(results, c.toTargetImage(payloadImg, dest, meta))

			// Extract component images from the payload's image-references layer.
			componentImages, extractErr := c.releaseResolver.ExtractComponentImages(ctx, payloadImg, arch[0])
			if extractErr != nil {
				fmt.Printf("Warning: failed to extract component images from %s: %v\n", payloadImg, extractErr)
				continue
			}
			for _, compImg := range componentImages {
				compDest := releaseDestination(target.Spec.Registry, effectiveMaxVersion, compImg)
				results = append(results, c.toTargetImage(compImg, compDest, meta))
			}
		}
	}

	// 2. Collect Operators
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
			tag := "latest"
			if strings.Contains(op.Catalog, ":") {
				tag = strings.Split(op.Catalog, ":")[len(strings.Split(op.Catalog, ":"))-1]
			}
			dest := catalogDestination(target.Spec.Registry, img, tag)
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

// releaseDestination builds a unique destination path for a release payload or
// component image. Component images come from quay.io/openshift-release-dev/ocp-v4.0-art-dev
// and are stored under the release version directory to avoid collisions.
func releaseDestination(registry, releaseVersion, img string) string {
	// Strip registry prefix from the image to keep only the path.
	imgNoTag := strings.Split(img, ":")[0]
	imgNoDigest := strings.Split(imgNoTag, "@")[0]

	parts := strings.SplitN(imgNoDigest, "/", 2)
	namePath := imgNoDigest
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		namePath = parts[1]
	}

	ver := releaseVersion
	if ver == "" {
		ver = "latest"
	}
	return fmt.Sprintf("%s/openshift/release/%s/%s", registry, ver, namePath)
}

// preserving the image's repository path (minus the source registry) so that
// different catalogs never overwrite each other in the target registry.
func catalogDestination(registry, catalogImage, tag string) string {
	imageNoTag := strings.Split(catalogImage, ":")[0]
	imageNoDigest := strings.Split(imageNoTag, "@")[0]

	parts := strings.SplitN(imageNoDigest, "/", 2)
	nameWithPath := imageNoDigest
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		nameWithPath = parts[1]
	}

	return fmt.Sprintf("%s/%s:%s", registry, nameWithPath, tag)
}
