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

// CollectTargetImages parses the ImageSet configuration and returns the list
// of images to mirror. It is a thin convenience wrapper that runs all three
// origin-scoped collectors (releases, operators, additional) and concatenates
// their results, used primarily by tests and the catalog-builder one-shot.
//
// In the production reconcile path the manager calls the per-origin methods
// directly so it can attach the correct ImageOrigin to each resulting entry
// when writing imagestate.
func (c *Collector) CollectTargetImages(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, meta *state.Metadata) ([]TargetImage, error) {
	var results []TargetImage

	rel, err := c.CollectReleases(ctx, spec, target, meta)
	if err != nil {
		return nil, err
	}
	results = append(results, rel...)

	op, err := c.CollectOperators(ctx, spec, target, meta)
	if err != nil {
		return nil, err
	}
	results = append(results, op...)

	add, err := c.CollectAdditional(ctx, spec, target, meta)
	if err != nil {
		return nil, err
	}
	results = append(results, add...)

	return results, nil
}

// CollectReleases resolves all OCP/OKD release payloads referenced via
// spec.Mirror.Platform.Channels and returns each payload + its ~190 extracted
// component images, plus optional KubeVirt container-disk images.
//
// All emitted TargetImages carry no Origin (caller-set; manager tags them
// OriginRelease before persisting to imagestate).
func (c *Collector) CollectReleases(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, meta *state.Metadata) ([]TargetImage, error) {
	var results []TargetImage
	for _, rel := range spec.Mirror.Platform.Channels {
		images, err := c.CollectReleasesForChannel(ctx, spec, target, rel, nil)
		if err != nil {
			fmt.Printf("Warning: collect channel %s: %v\n", rel.Name, err)
			continue
		}
		results = append(results, images...)
	}
	return results, nil
}

// ResolveReleasePayloadImages performs the cheap channel→payload-image-list
// resolution (Cincinnati graph traversal) without doing the expensive
// per-payload component image extraction. The result is suitable for caching
// signature computation (release.ResolvedSignature) and is reused as input to
// CollectReleasesForChannel to avoid double work.
func (c *Collector) ResolveReleasePayloadImages(ctx context.Context, rel mirrorv1alpha1.ReleaseChannel, arch []string) ([]string, error) {
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}
	effectiveMaxVersion := rel.MaxVersion
	if effectiveMaxVersion == "" && !rel.Full && rel.MinVersion == "" {
		resolved, resolveErr := c.releaseResolver.ResolveLatestVersion(ctx, rel.Name, arch)
		if resolveErr == nil {
			effectiveMaxVersion = resolved
		}
	}
	return c.releaseResolver.ResolveRelease(ctx, rel.Name, rel.MinVersion, effectiveMaxVersion, arch, rel.Full, rel.ShortestPath)
}

// CollectReleasesForChannel resolves a single release channel and extracts
// component + KubeVirt images. If payloadImages is non-nil it is used as-is
// (avoids an extra Cincinnati round-trip when the caller already invoked
// ResolveReleasePayloadImages for caching purposes).
func (c *Collector) CollectReleasesForChannel(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, rel mirrorv1alpha1.ReleaseChannel, payloadImages []string) ([]TargetImage, error) {
	var results []TargetImage
	arch := spec.Mirror.Platform.Architectures
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}

	effectiveMaxVersion := rel.MaxVersion
	if effectiveMaxVersion == "" && !rel.Full && rel.MinVersion == "" {
		resolved, resolveErr := c.releaseResolver.ResolveLatestVersion(ctx, rel.Name, arch)
		if resolveErr != nil {
			fmt.Printf("Warning: failed to resolve latest version for channel %s: %v\n", rel.Name, resolveErr)
		} else {
			effectiveMaxVersion = resolved
		}
	}

	if payloadImages == nil {
		var err error
		payloadImages, err = c.releaseResolver.ResolveRelease(ctx, rel.Name, rel.MinVersion, effectiveMaxVersion, arch, rel.Full, rel.ShortestPath)
		if err != nil {
			return nil, fmt.Errorf("resolve release %s: %w", rel.Name, err)
		}
	}

	for _, payloadImg := range payloadImages {
		dest := releasePayloadDestination(target.Spec.Registry, effectiveMaxVersion, payloadImg)
		results = append(results, c.toTargetImage(payloadImg, dest, nil))

		componentImages, extractErr := c.releaseResolver.ExtractComponentImages(ctx, payloadImg, arch[0])
		if extractErr != nil {
			fmt.Printf("Warning: failed to extract component images from %s: %v\n", payloadImg, extractErr)
			continue
		}
		for _, compImg := range componentImages {
			compDest := componentDestination(target.Spec.Registry, compImg)
			results = append(results, c.toTargetImage(compImg, compDest, nil))
		}

		if spec.Mirror.Platform.KubeVirtContainer {
			kvImages, kvErr := c.releaseResolver.ExtractKubeVirtImages(ctx, payloadImg, arch)
			if kvErr != nil {
				fmt.Printf("Warning: failed to extract KubeVirt images from %s: %v\n", payloadImg, kvErr)
			} else {
				for _, kvImg := range kvImages {
					kvDest := componentDestination(target.Spec.Registry, kvImg)
					results = append(results, c.toTargetImage(kvImg, kvDest, nil))
				}
			}
		}
	}
	return results, nil
}

// CollectOperators resolves all operator catalogs referenced via
// spec.Mirror.Operators and returns the union of bundle + related images that
// need mirroring. The filtered catalog itself is pushed by the
// CatalogBuildJob and is NOT included here.
//
// Each catalog is pulled and its FBC parsed. This requires registry
// credentials for every distinct catalog source, which the manager pod
// supplies via DOCKER_CONFIG.
func (c *Collector) CollectOperators(ctx context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, meta *state.Metadata) ([]TargetImage, error) {
	var results []TargetImage
	for _, op := range spec.Mirror.Operators {
		images, err := c.CollectOperatorEntry(ctx, op, target)
		if err != nil {
			fmt.Printf("Warning: failed to resolve catalog %s: %v\n", op.Catalog, err)
			continue
		}
		results = append(results, images...)
	}
	return results, nil
}

// CollectOperatorEntry resolves a single Operator entry. Used by the manager
// when a per-entry cache miss requires re-resolution.
func (c *Collector) CollectOperatorEntry(ctx context.Context, op mirrorv1alpha1.Operator, target *mirrorv1alpha1.MirrorTarget) ([]TargetImage, error) {
	pkgs := []string{}
	for _, p := range op.Packages {
		pkgs = append(pkgs, p.Name)
	}
	images, err := c.catalogResolver.ResolveCatalog(ctx, op.Catalog, pkgs)
	if err != nil {
		return nil, err
	}
	var results []TargetImage
	for _, img := range images {
		dest := componentDestination(target.Spec.Registry, img)
		results = append(results, c.toTargetImage(img, dest, nil))
	}
	return results, nil
}

// CollectAdditional returns the destination entries for every image listed in
// spec.Mirror.AdditionalImages. No upstream resolution is needed because the
// user has already supplied the fully-qualified source reference.
func (c *Collector) CollectAdditional(_ context.Context, spec *mirrorv1alpha1.ImageSetSpec, target *mirrorv1alpha1.MirrorTarget, meta *state.Metadata) ([]TargetImage, error) {
	var results []TargetImage
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
//
//	e.g. quay.io/openshift-release-dev/ocp-release@sha256:abc → registry/openshift-release-dev/ocp-release:4.21.9
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
//
//	e.g. quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:184844… →
//	     registry/openshift-release-dev/ocp-v4.0-art-dev:sha256-184844…
func componentDestination(registry, img string) string {
	namePath := imageNamePath(img)
	// Derive a stable tag from the digest so each component gets a unique destination.
	if idx := strings.Index(img, "@sha256:"); idx >= 0 {
		tag := "sha256-" + img[idx+8:] // "sha256-{hex}"
		return fmt.Sprintf("%s/%s:%s", registry, namePath, tag)
	}
	return fmt.Sprintf("%s/%s", registry, namePath)
}
