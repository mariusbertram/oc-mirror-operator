package mirror

import (
	"context"
	"fmt"

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

		images, err := c.releaseResolver.ResolveRelease(ctx, rel.Name, rel.MaxVersion, arch)
		if err != nil {
			fmt.Printf("Warning: failed to resolve release %s/%s: %v\n", rel.Name, rel.MaxVersion, err)
			continue
		}
		for _, img := range images {
			dest := fmt.Sprintf("%s/openshift/release:%s", target.Spec.Registry, rel.MaxVersion)
			results = append(results, c.toTargetImage(img, dest, meta))
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
			dest := fmt.Sprintf("%s/operator-catalog:%s", target.Spec.Registry, "latest")
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
