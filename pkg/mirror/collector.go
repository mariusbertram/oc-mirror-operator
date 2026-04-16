package mirror

import (
	"context"
	"fmt"
	mirrorv1alpha1 "github.com/mariusbertram/ocp-mirror/api/v1alpha1"
)

// TargetImage defines a source and destination for mirroring
type TargetImage struct {
	Source      string
	Destination string
}

// Collector gathers the list of target images from an ImageSet configuration
type Collector struct {
	client *MirrorClient
}

func NewCollector(client *MirrorClient) *Collector {
	return &Collector{client: client}
}

// CollectTargetImages parses the ImageSet configuration and returns the list of images to mirror
func (c *Collector) CollectTargetImages(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget) ([]TargetImage, error) {
	var results []TargetImage

	// 1. Collect AdditionalImages
	for _, img := range spec.Mirror.AdditionalImages {
		dest := fmt.Sprintf("%s/%s", target.Spec.Registry, img.Name)
		if img.TargetRepo != "" {
			dest = fmt.Sprintf("%s/%s", target.Spec.Registry, img.TargetRepo)
		}
		if img.TargetTag != "" {
			// Replace or add tag
			// Simplified: just append if not exists or replace
			dest = dest + ":" + img.TargetTag
		}
		results = append(results, TargetImage{
			Source:      img.Name,
			Destination: dest,
		})
	}

	// 2. Collect Platform Images (Simplification: only head for now)
	// In reality, this needs to query the Cincinnati API or release payloads
	// We will implement this more thoroughly in a real scenario using oc-mirror/v2 libs.

	// 3. Collect Operator Images (OLM Catalog)
	// This involves parsing the catalog and finding images for included packages.

	return results, nil
}
