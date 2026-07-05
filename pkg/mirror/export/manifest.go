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

// Package export resolves a MirrorExport's content into the artifacts
// described in https://github.com/mariusbertram/oc-mirror-operator/issues/82:
// a resolved image manifest for plain (copyable) images, IDMS/ITMS/
// CatalogSource/ClusterCatalog resources, and a build spec describing the
// operator-catalog/graph-data content that must be built rather than copied.
//
// Resolution reuses pkg/mirror/collector.go and its release/catalog/helm
// resolvers unchanged — a MirrorExport asks the exact same question an
// ImageSet does ("what images does this content need"), it just renders the
// answer as downloadable artifacts instead of driving Manager/worker pods.
package export

import (
	"context"
	"fmt"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// ManifestEntry is one plain (directly copyable) image resolved from the
// MirrorExport's content. Origin/BundleRef are metadata only — they do not
// affect how the entry is copied.
type ManifestEntry struct {
	// Source is the fully-qualified upstream (or Source-override) image reference.
	Source string `json:"source"`
	// Destination is the fully-qualified image reference in Spec.Destination.Registry.
	Destination string `json:"destination"`
	// Origin identifies which part of the spec produced this entry (release,
	// operator, additional, helm) — see imagestate.ImageOrigin.
	Origin imagestate.ImageOrigin `json:"origin,omitempty"`
	// BundleRef lists the OLM bundle(s) that reference this image, when Origin
	// is "operator" (see mirror.TargetImage.BundleRef).
	BundleRef string `json:"bundleRef,omitempty"`
}

// Manifest is the resolved, absolute list of plain images a MirrorExport
// needs copied — every entry is already a concrete image reference; no
// further version/channel resolution is needed to act on it.
type Manifest struct {
	Images []ManifestEntry `json:"images"`
}

// BuildManifest resolves spec's releases, operator bundle images (the
// catalog overlay itself is excluded — see BuildSpec), additional images,
// and Helm chart images against sourceRegistry (as target.Spec.Registry is
// repurposed here) — i.e. the Collector's normal target parameter — and
// returns them tagged with their origin, blocked-image filtering applied.
//
// destRegistry is used only to compute destination references; the resolving
// process does not need network access to it.
func BuildManifest(ctx context.Context, c *mirror.Collector, spec *mirrorv1alpha1.ImageSetSpec, destRegistry string) (Manifest, error) {
	target := &mirrorv1alpha1.MirrorTarget{
		Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: destRegistry},
	}

	var entries []ManifestEntry

	releases, err := c.CollectReleases(ctx, spec, target, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve releases: %w", err)
	}
	entries = append(entries, tagOrigin(releases, imagestate.OriginRelease)...)

	operators, err := c.CollectOperators(ctx, spec, target, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve operator bundle images: %w", err)
	}
	entries = append(entries, tagOrigin(operators, imagestate.OriginOperator)...)

	additional, err := c.CollectAdditional(ctx, spec, target, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve additional images: %w", err)
	}
	entries = append(entries, tagOrigin(additional, imagestate.OriginAdditional)...)

	helmImages, err := c.CollectHelm(ctx, spec, target, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve helm images: %w", err)
	}
	entries = append(entries, tagOrigin(helmImages, imagestate.OriginHelm)...)

	entries = blockEntries(entries, spec.Mirror.BlockedImages)

	return Manifest{Images: entries}, nil
}

func tagOrigin(images []mirror.TargetImage, origin imagestate.ImageOrigin) []ManifestEntry {
	entries := make([]ManifestEntry, len(images))
	for i, img := range images {
		entries[i] = ManifestEntry{
			Source:      img.Source,
			Destination: img.Destination,
			Origin:      origin,
			BundleRef:   img.BundleRef,
		}
	}
	return entries
}

// blockEntries removes any entry whose Source matches a configured
// spec.mirror.blockedImages name, mirroring mirror.BlockImages but operating
// on ManifestEntry (which carries Origin, unlike mirror.TargetImage).
func blockEntries(entries []ManifestEntry, blocked []mirrorv1alpha1.BlockedImage) []ManifestEntry {
	if len(blocked) == 0 {
		return entries
	}
	blockedPaths := make(map[string]struct{}, len(blocked))
	for _, b := range blocked {
		blockedPaths[mirror.ImageNamePath(b.Name)] = struct{}{}
	}
	filtered := make([]ManifestEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := blockedPaths[mirror.ImageNamePath(e.Source)]; ok {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// ToImageState builds a synthetic imagestate.ImageState from the manifest so
// existing IDMS/ITMS generation (pkg/mirror/resources) can be reused
// unchanged. Every entry is marked "Mirrored": the generated resources
// describe the mirror mapping the destination registry is expected to have
// once the resolved images have actually been copied there (by regctl,
// outside the cluster) — not the current state of the exporting cluster,
// which never touches the image bytes at all.
func (m Manifest) ToImageState(exportName string) imagestate.ImageState {
	state := make(imagestate.ImageState, len(m.Images))
	for _, e := range m.Images {
		entry, ok := state[e.Destination]
		if !ok {
			entry = &imagestate.ImageEntry{
				Source: e.Source,
				State:  "Mirrored",
			}
			state[e.Destination] = entry
		}
		entry.AddRef(imagestate.ImageRef{
			ImageSet: exportName,
			Origin:   e.Origin,
		})
	}
	return state
}
