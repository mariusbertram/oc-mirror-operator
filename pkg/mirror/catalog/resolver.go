package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"github.com/blang/semver/v4"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	godigest "github.com/opencontainers/go-digest"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

// configsPath is the standard FBC directory inside a catalog image.
const configsPath = "configs/"

// cachePath is the path opm uses for its pre-built pogreb cache. Source
// catalog images ship a pre-built cache here that is keyed to the unfiltered
// catalog content; once we replace /configs the cache is stale, so any layer
// that contains it must be either skipped or whited out.
const cachePath = "tmp/cache/"

// classifyLayer streams a gzipped tar layer and returns true when *every*
// regular-file entry in the layer lies under either configsPath or cachePath.
// Such layers can be safely skipped when building a filtered catalog overlay,
// because the new overlay layer fully replaces both directories. Directory
// entries are ignored (a layer that only ships directories has no payload to
// preserve). Hardlinks/symlinks targeting outside paths are treated as
// non-skippable to stay on the safe side.
//
// totalSize is the sum of regular-file sizes inside configs/ + tmp/cache/
// (used only for logging).
//
// firstReject (when non-empty) is the first tar entry name that disqualified
// the layer from being skippable — useful for diagnostic logging.
func classifyLayer(r io.Reader) (skippable bool, totalSize int64, firstReject string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return false, 0, "", fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	sawAny := false
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return false, 0, "", fmt.Errorf("tar: %w", nextErr)
		}

		name := strings.TrimPrefix(hdr.Name, "./")

		// Pure directory entries carry no payload — ignore for classification.
		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		// Anything outside the two known FBC paths disqualifies the layer.
		if !strings.HasPrefix(name, configsPath) && !strings.HasPrefix(name, cachePath) {
			return false, 0, name, nil
		}

		// Refuse to skip layers that contain hardlinks/symlinks (their targets
		// could escape configs/) or other non-regular file types.
		if hdr.Typeflag != tar.TypeReg {
			return false, 0, name + " (non-regular file)", nil
		}

		sawAny = true
		totalSize += hdr.Size
	}

	return sawAny, totalSize, "", nil
}

// classifyResult holds the output of classifying all layers in a catalog image.
type classifyResult struct {
	keptLayers  []descriptor.Descriptor
	keptDiffIDs []godigest.Digest
	configFS    fstest.MapFS
}

// classifySourceLayers reads every layer from a local OCI layout ref, classifies
// each as FBC-only (skippable) or containing non-FBC content (kept), and
// simultaneously extracts FBC config files into the returned MapFS.
func classifySourceLayers(ctx context.Context, client *mirrorclient.MirrorClient, localRef ref.Ref,
	srcLayers []descriptor.Descriptor, srcDiffIDs []godigest.Digest, imageLabel string) *classifyResult {

	keptLayers := make([]descriptor.Descriptor, 0, len(srcLayers))
	keptDiffIDs := make([]godigest.Digest, 0, len(srcDiffIDs))
	configFS := make(fstest.MapFS)
	var skippedCount int
	var skippedBytes int64
	for i, layer := range srcLayers {
		blobRdr, blobErr := client.BlobGet(ctx, localRef, layer)
		if blobErr != nil {
			slog.WarnContext(ctx, "cannot classify source layer, will copy",
				"image", imageLabel, "digest", layer.Digest.String(), "error", blobErr)
			keptLayers = append(keptLayers, layer)
			keptDiffIDs = append(keptDiffIDs, srcDiffIDs[i])
			continue
		}
		skip, sz, firstReject, classifyErr := classifyAndExtractFBC(blobRdr, configFS)
		_ = blobRdr.Close()
		if classifyErr != nil {
			slog.WarnContext(ctx, "layer classification failed, will copy",
				"image", imageLabel, "digest", layer.Digest.String(), "error", classifyErr)
			keptLayers = append(keptLayers, layer)
			keptDiffIDs = append(keptDiffIDs, srcDiffIDs[i])
			continue
		}
		if skip {
			skippedCount++
			skippedBytes += layer.Size
			slog.InfoContext(ctx, "skipping source FBC/cache-only layer",
				"image", imageLabel,
				"digest", layer.Digest.String(),
				"compressed_bytes", layer.Size,
				"uncompressed_bytes", sz)
			continue
		}
		slog.InfoContext(ctx, "keeping source layer (non-FBC content)",
			"image", imageLabel,
			"digest", layer.Digest.String(),
			"compressed_bytes", layer.Size,
			"first_non_fbc_entry", firstReject)
		keptLayers = append(keptLayers, layer)
		keptDiffIDs = append(keptDiffIDs, srcDiffIDs[i])
	}
	if skippedCount > 0 {
		slog.InfoContext(ctx, "filtered catalog: dropped fully-replaced source layers",
			"skipped_layers", skippedCount,
			"kept_layers", len(keptLayers),
			"saved_compressed_bytes", skippedBytes)
	}
	return &classifyResult{keptLayers: keptLayers, keptDiffIDs: keptDiffIDs, configFS: configFS}
}

// resolveManifestList resolves a manifest list to a single linux/amd64 manifest.
// If the manifest is already a single image, it is returned as-is along with the
// unmodified ref. Otherwise the ref is updated to point at the platform digest.
func resolveManifestList(ctx context.Context, client *mirrorclient.MirrorClient,
	localRef ref.Ref, m manifest.Manifest, imageLabel string) (ref.Ref, manifest.Manifest, error) {

	if !m.IsList() {
		return localRef, m, nil
	}
	p, err := platform.Parse("linux/amd64")
	if err != nil {
		return localRef, nil, fmt.Errorf("failed to parse platform: %w", err)
	}
	desc, err := manifest.GetPlatformDesc(m, &p)
	if err != nil {
		return localRef, nil, fmt.Errorf("no linux/amd64 manifest in %s: %w", imageLabel, err)
	}
	localRef.Digest = desc.Digest.String()
	localRef.Tag = ""
	resolved, err := client.ManifestGet(ctx, localRef)
	if err != nil {
		return localRef, nil, fmt.Errorf("failed to get platform manifest: %w", err)
	}
	return localRef, resolved, nil
}

// classifyAndExtractFBC combines layer classification with FBC content
// extraction in a single streaming pass. It reads the entire gzipped tar
// stream once, determines whether the layer is skippable (FBC-only), and
// simultaneously extracts all configs/ entries into configFS.
//
// This avoids the need to download the same blob twice (once for
// classification, once for FBC extraction in loadFBCFromImage).
func classifyAndExtractFBC(r io.Reader, configFS fstest.MapFS) (skippable bool, totalSize int64, firstReject string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return false, 0, "", fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	sawAny := false
	fbcOnly := true

	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return false, 0, "", fmt.Errorf("tar: %w", nextErr)
		}

		name := strings.TrimPrefix(hdr.Name, "./")

		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		// Extract FBC files regardless of whether the layer is skippable,
		// because the opaque whiteout replaces configs/ from all layers.
		if hdr.Typeflag == tar.TypeReg && strings.HasPrefix(name, configsPath) {
			data, readErr := io.ReadAll(io.LimitReader(tr, 64*1024*1024))
			if readErr == nil {
				configFS[name] = &fstest.MapFile{Data: data}
			}
		}

		if !strings.HasPrefix(name, configsPath) && !strings.HasPrefix(name, cachePath) {
			if fbcOnly {
				firstReject = name
				fbcOnly = false
			}
			continue
		}

		if hdr.Typeflag != tar.TypeReg {
			if fbcOnly {
				firstReject = name + " (non-regular file)"
				fbcOnly = false
			}
			continue
		}

		sawAny = true
		totalSize += hdr.Size
	}

	return sawAny && fbcOnly, totalSize, firstReject, nil
}

// blobCopyWithRetry transfers a blob from src to dst using BlobGet + BlobPut.
// This explicit read-then-write approach is used instead of BlobCopy because
// regclient's BlobCopy may attempt server-side blob mounts that hang when
// copying between different schemes (e.g. ocidir:// → registry://).
// Each attempt is given its own context deadline so a stalled transfer does not
// consume the entire parent context budget.
func blobCopyWithRetry(ctx context.Context, client *mirrorclient.MirrorClient, src, dst ref.Ref, d descriptor.Descriptor, maxAttempts int, perAttemptTimeout time.Duration) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * 5 * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			slog.Info("retrying blob transfer",
				"attempt", attempt, "digest", d.Digest.String(), "error", lastErr,
				"time", time.Now().Format("15:04:05"))
		}
		attemptStart := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		lastErr = blobGetAndPut(attemptCtx, client, src, dst, d)
		cancel()
		if lastErr == nil {
			slog.Info("blob transfer complete",
				"digest", d.Digest.String()[:16], "bytes", d.Size,
				"duration", time.Since(attemptStart).Round(time.Millisecond).String())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

// blobGetAndPut reads a blob from src and pushes it to dst.
func blobGetAndPut(ctx context.Context, client *mirrorclient.MirrorClient, src, dst ref.Ref, d descriptor.Descriptor) error {
	rdr, err := client.BlobGet(ctx, src, d)
	if err != nil {
		return fmt.Errorf("BlobGet %s: %w", d.Digest.String(), err)
	}
	defer func() { _ = rdr.Close() }()
	_, err = client.BlobPut(ctx, dst, d, rdr)
	if err != nil {
		return fmt.Errorf("BlobPut %s: %w", d.Digest.String(), err)
	}
	return nil
}

type CatalogResolver struct {
	client *mirrorclient.MirrorClient
}

func New(client *mirrorclient.MirrorClient) *CatalogResolver {
	return &CatalogResolver{client: client}
}

// GetCatalogDigest performs a lightweight manifest lookup against the catalog
// source registry and returns the upstream digest (e.g. "sha256:abc…").
//
// This is the cheap caching probe used by the manager: if the returned digest
// matches a previously cached value (stored as an annotation on the ImageSet),
// the manager skips the expensive ResolveCatalog call entirely.
//
// If the source reference already pins a digest, that digest is returned
// without a network round-trip.
func (r *CatalogResolver) GetCatalogDigest(ctx context.Context, catalogImage string) (string, error) {
	parsed, err := ref.New(catalogImage)
	if err != nil {
		return "", fmt.Errorf("parse catalog image reference: %w", err)
	}
	if parsed.Digest != "" {
		return parsed.Digest, nil
	}
	if r.client == nil {
		return "", fmt.Errorf("no client configured")
	}
	return r.client.GetDigest(ctx, catalogImage)
}

// ResolveCatalog pulls the catalog image, extracts the File-Based Catalog (FBC)
// from the image layers, filters it to the requested packages (including
// transitive dependencies), and returns all bundle + related images.
//
// The source catalog image itself is NOT included — the filtered catalog is
// built and pushed separately by the CatalogBuildJob.
func (r *CatalogResolver) ResolveCatalog(ctx context.Context, catalogImage string, packages []string) ([]string, error) {
	if _, err := ref.New(catalogImage); err != nil {
		return nil, fmt.Errorf("failed to parse catalog image reference: %w", err)
	}

	if r.client == nil {
		return nil, nil
	}

	// If no packages requested, return only the catalog index image itself.
	if len(packages) == 0 {
		return []string{catalogImage}, nil
	}

	cfg, err := r.loadFBCFromImage(ctx, catalogImage)
	if err != nil {
		return nil, fmt.Errorf("failed to load FBC from %s: %w", catalogImage, err)
	}

	includes := make([]mirrorv1alpha1.IncludePackage, 0, len(packages))
	for _, p := range packages {
		includes = append(includes, mirrorv1alpha1.IncludePackage{Name: p})
	}

	filtered, err := r.FilterFBC(ctx, cfg, includes)
	if err != nil {
		return nil, fmt.Errorf("failed to filter FBC: %w", err)
	}

	fmt.Printf("Catalog %s: filtered to %d packages, %d channels, %d bundles\n",
		catalogImage, len(filtered.Packages), len(filtered.Channels), len(filtered.Bundles))

	// Always include the catalog index image itself so it gets mirrored.
	extracted := r.ExtractImages(filtered)
	result := make([]string, 0, 1+len(extracted))
	result = append(result, catalogImage)
	result = append(result, extracted...)
	return result, nil
}

// loadFBCFromImage fetches all image layers, collects every file under configs/,
// and parses them as a DeclarativeConfig using declcfg.LoadFS.
//
// Layers are applied in order (first → last) so that overlay semantics are
// preserved: a file in a later layer overrides the same path from an earlier one.
func (r *CatalogResolver) loadFBCFromImage(ctx context.Context, catalogImage string) (*declcfg.DeclarativeConfig, error) {
	imgRef, err := ref.New(catalogImage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse image reference: %w", err)
	}

	m, err := r.client.ManifestGet(ctx, imgRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest for %s: %w", catalogImage, err)
	}

	// Resolve to the amd64 platform manifest if we got a manifest list.
	if m.IsList() {
		p, parseErr := platform.Parse("linux/amd64")
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse platform: %w", parseErr)
		}
		desc, descErr := manifest.GetPlatformDesc(m, &p)
		if descErr != nil {
			return nil, fmt.Errorf("no amd64 manifest in %s: %w", catalogImage, descErr)
		}
		imgRef.Digest = desc.Digest.String()
		imgRef.Tag = ""
		m, err = r.client.ManifestGet(ctx, imgRef)
		if err != nil {
			return nil, fmt.Errorf("failed to get platform manifest: %w", err)
		}
	}

	layers, err := m.GetLayers() //nolint:staticcheck
	if err != nil {
		return nil, fmt.Errorf("failed to get layers: %w", err)
	}

	// Build an in-memory overlay FS from all layers.
	// Later layers override earlier ones (standard OCI overlay semantics).
	configFS := make(fstest.MapFS)
	var blobErrs int
	for _, layer := range layers {
		blobRdr, blobErr := r.client.BlobGet(ctx, imgRef, layer)
		if blobErr != nil {
			blobErrs++
			slog.WarnContext(ctx, "skipping unreadable catalog layer",
				"image", catalogImage, "digest", layer.Digest.String(), "error", blobErr)
			continue
		}
		if extractedFiles := extractFBCLayer(blobRdr, configFS); extractedFiles == 0 {
			slog.DebugContext(ctx, "no FBC files in layer",
				"image", catalogImage, "digest", layer.Digest.String())
		}
		_ = blobRdr.Close()
	}

	if len(configFS) == 0 {
		if blobErrs > 0 {
			return nil, fmt.Errorf("no FBC config files found under %s in %s (failed to read %d/%d layers)",
				configsPath, catalogImage, blobErrs, len(layers))
		}
		return nil, fmt.Errorf("no FBC config files found under %s in %s", configsPath, catalogImage)
	}

	// declcfg.LoadFS expects the root to be the configs/ directory.
	// Since our keys are like "configs/argo-cd/catalog.yaml", we create a
	// sub-FS rooted at "configs".
	subFS, err := fs.Sub(configFS, strings.TrimSuffix(configsPath, "/"))
	if err != nil {
		return nil, fmt.Errorf("failed to create configs sub-fs: %w", err)
	}

	return declcfg.LoadFS(ctx, subFS)
}

// extractFBCLayer reads a gzipped tar layer and copies every regular file found
// under configs/ into fsMap. Returns the number of config files found.
func extractFBCLayer(r io.Reader, fsMap fstest.MapFS) int {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0
	}
	defer func() { _ = gz.Close() }()

	count := 0
	tr := tar.NewReader(gz)
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			break
		}

		// Normalize the path: remove leading "./".
		name := strings.TrimPrefix(hdr.Name, "./")

		if !strings.HasPrefix(name, configsPath) {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Cap individual file reads to 64 MiB to prevent OOM when a malicious
		// or corrupt catalog layer claims an enormous file size.
		data, readErr := io.ReadAll(io.LimitReader(tr, 64*1024*1024))
		if readErr != nil {
			continue
		}

		fsMap[name] = &fstest.MapFile{Data: data}
		count++
	}
	return count
}

// OLM property types for dependency resolution.
const (
	olmPackageRequired = "olm.package.required"
	olmGVKRequired     = "olm.gvk.required"
	olmGVK             = "olm.gvk"
	olmPackage         = "olm.package"
)

// packageRequiredValue is the JSON structure for an olm.package.required property.
type packageRequiredValue struct {
	PackageName string `json:"packageName"`
}

// gvkValue is the JSON structure for olm.gvk and olm.gvk.required properties.
type gvkValue struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// gvkKey returns a unique string key for indexing.
func (g gvkValue) gvkKey() string {
	return g.Group + "/" + g.Version + "/" + g.Kind
}

// bundleVersion extracts the version string from a bundle's olm.package property.
// Returns "" if the version cannot be determined.
func bundleVersion(b declcfg.Bundle) string {
	for _, prop := range b.Properties {
		if prop.Type != olmPackage {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(prop.Value, &v) == nil && v.Version != "" {
			return v.Version
		}
	}
	return ""
}

// channelVersionFilter holds the effective semver range for bundles within a
// specific channel (channel-level values override package-level values).
type channelVersionFilter struct {
	hasMinVer bool
	hasMaxVer bool
	minSV     semver.Version
	maxSV     semver.Version
}

// pkgIncludeFilter holds the resolved filter constraints for one explicitly
// requested operator package.
type pkgIncludeFilter struct {
	allowAllChannels bool
	allowedChannels  map[string]bool
	channelFilters   map[string]channelVersionFilter
	pkgHasMinVer     bool
	pkgHasMaxVer     bool
	pkgMinSV         semver.Version
	pkgMaxSV         semver.Version
}

// buildPkgIncludeFilter constructs a pkgIncludeFilter from an IncludePackage spec.
func buildPkgIncludeFilter(inc mirrorv1alpha1.IncludePackage) pkgIncludeFilter {
	f := pkgIncludeFilter{
		allowedChannels: make(map[string]bool),
		channelFilters:  make(map[string]channelVersionFilter),
	}

	if inc.MinVersion != "" {
		if sv, err := semver.ParseTolerant(inc.MinVersion); err == nil {
			f.pkgMinSV = sv
			f.pkgHasMinVer = true
		}
	}
	if inc.MaxVersion != "" {
		if sv, err := semver.ParseTolerant(inc.MaxVersion); err == nil {
			f.pkgMaxSV = sv
			f.pkgHasMaxVer = true
		}
	}

	if len(inc.Channels) == 0 {
		f.allowAllChannels = true
	} else {
		for _, ch := range inc.Channels {
			f.allowedChannels[ch.Name] = true
			cf := channelVersionFilter{}
			if ch.MinVersion != "" {
				if sv, err := semver.ParseTolerant(ch.MinVersion); err == nil {
					cf.minSV = sv
					cf.hasMinVer = true
				}
			}
			if ch.MaxVersion != "" {
				if sv, err := semver.ParseTolerant(ch.MaxVersion); err == nil {
					cf.maxSV = sv
					cf.hasMaxVer = true
				}
			}
			f.channelFilters[ch.Name] = cf
		}
	}
	return f
}

// effectiveVersionFilter returns the version range that applies to a channel,
// giving channel-level filters priority over package-level ones.
func (f pkgIncludeFilter) effectiveVersionFilter(chName string) channelVersionFilter {
	cf, hasCh := f.channelFilters[chName]
	var result channelVersionFilter

	// Min version: channel-level takes priority, then package-level.
	if hasCh && cf.hasMinVer {
		result.minSV = cf.minSV
		result.hasMinVer = true
	} else if f.pkgHasMinVer {
		result.minSV = f.pkgMinSV
		result.hasMinVer = true
	}

	// Max version: same priority.
	if hasCh && cf.hasMaxVer {
		result.maxSV = cf.maxSV
		result.hasMaxVer = true
	} else if f.pkgHasMaxVer {
		result.maxSV = f.pkgMaxSV
		result.hasMaxVer = true
	}
	return result
}

// bundleMatchesVersionFilter returns true if the bundle should be included
// given the resolved version filter. If the bundle's version cannot be
// determined, it is included (lenient behaviour to avoid breaking catalogs).
func bundleMatchesVersionFilter(b declcfg.Bundle, vf channelVersionFilter) bool {
	if !vf.hasMinVer && !vf.hasMaxVer {
		return true // no filter
	}
	ver := bundleVersion(b)
	if ver == "" {
		return true // unknown version → include
	}
	sv, err := semver.ParseTolerant(ver)
	if err != nil {
		return true // unparseable → include
	}
	if vf.hasMinVer && sv.LT(vf.minSV) {
		return false
	}
	if vf.hasMaxVer && sv.GT(vf.maxSV) {
		return false
	}
	return true
}

// channelHeadPlusN returns the head bundle(s) of a channel plus up to
// `previous` older entries walking backwards through the Replaces chain.
// With previous=0 only the head(s) are returned.
func channelHeadPlusN(ch declcfg.Channel, previous int) []string {
	if len(ch.Entries) == 0 {
		return nil
	}
	superseded := make(map[string]bool, len(ch.Entries))
	for _, e := range ch.Entries {
		if e.Replaces != "" {
			superseded[e.Replaces] = true
		}
		for _, s := range e.Skips {
			superseded[s] = true
		}
	}
	var heads []string
	for _, e := range ch.Entries {
		if !superseded[e.Name] {
			heads = append(heads, e.Name)
		}
	}
	if len(heads) == 0 {
		// Safety fallback: return last entry.
		heads = []string{ch.Entries[len(ch.Entries)-1].Name}
	}

	if previous <= 0 {
		return heads
	}

	// Walk backwards from each head through the Replaces chain.
	entryByName := make(map[string]declcfg.ChannelEntry, len(ch.Entries))
	for _, e := range ch.Entries {
		entryByName[e.Name] = e
	}
	include := make(map[string]bool, len(heads)*(previous+1))
	for _, head := range heads {
		include[head] = true
		current := head
		for i := 0; i < previous; i++ {
			entry, ok := entryByName[current]
			if !ok || entry.Replaces == "" {
				break
			}
			include[entry.Replaces] = true
			current = entry.Replaces
		}
	}

	// Return in original entry order.
	var result []string
	for _, e := range ch.Entries {
		if include[e.Name] {
			result = append(result, e.Name)
		}
	}
	return result
}

// FilterFBC implements the in-memory filtering of a declarative configuration.
// It includes transitive dependencies by resolving both olm.package.required
// and olm.gvk.required properties from bundles of selected packages.
//
// When packages specify Channels or version ranges (MinVersion/MaxVersion),
// only the matching channels and bundles are retained. Transitively discovered
// dependency packages are always included in full (no filtering applied).
//
// oc-mirror v2 compatible behaviour: when no channels or version filters are
// specified for a package, only the channel head (latest version) of every
// channel is included ("heads-only" mode).
func (r *CatalogResolver) FilterFBC(ctx context.Context, cfg *declcfg.DeclarativeConfig, includes []mirrorv1alpha1.IncludePackage) (*declcfg.DeclarativeConfig, error) { //nolint:gocyclo
	if len(includes) == 0 {
		return cfg, nil
	}

	// Build index of which packages exist in the full catalog.
	catalogPkgs := make(map[string]bool, len(cfg.Packages))
	defaultChannelByPkg := make(map[string]string, len(cfg.Packages))
	for _, p := range cfg.Packages {
		catalogPkgs[p.Name] = true
		defaultChannelByPkg[p.Name] = p.DefaultChannel
	}

	// Build the set of explicitly requested packages and auto-discover companions.
	// Companions are treated as explicit so they get heads-only filtering by default.
	pkgSet := make(map[string]bool, len(includes))
	allIncludes := make([]mirrorv1alpha1.IncludePackage, 0, len(includes)*2)
	for _, inc := range includes {
		if !pkgSet[inc.Name] {
			pkgSet[inc.Name] = true
			allIncludes = append(allIncludes, inc)
		}
	}
	for _, inc := range includes {
		candidates := []string{inc.Name + "-dependencies", inc.Name + "-dependency", inc.Name + "-deps"}
		if strings.HasSuffix(inc.Name, "-operator") {
			base := strings.TrimSuffix(inc.Name, "-operator")
			candidates = append(candidates, base+"-dependencies")
		}
		for _, c := range candidates {
			if catalogPkgs[c] && !pkgSet[c] {
				pkgSet[c] = true
				allIncludes = append(allIncludes, mirrorv1alpha1.IncludePackage{Name: c})
				fmt.Printf("Including companion dependency package: %s (for %s)\n", c, inc.Name)
				break
			}
		}
	}

	bundlesByPkg := make(map[string][]declcfg.Bundle)
	bundlesByName := make(map[string]declcfg.Bundle, len(cfg.Bundles))
	for _, b := range cfg.Bundles {
		bundlesByPkg[b.Package] = append(bundlesByPkg[b.Package], b)
		bundlesByName[b.Name] = b
	}

	channelsByPkg := make(map[string][]declcfg.Channel)
	channelNamesByPkg := make(map[string]map[string]bool)
	for _, c := range cfg.Channels {
		channelsByPkg[c.Package] = append(channelsByPkg[c.Package], c)
		if channelNamesByPkg[c.Package] == nil {
			channelNamesByPkg[c.Package] = make(map[string]bool)
		}
		channelNamesByPkg[c.Package][c.Name] = true
	}

	// Build GVK provider index: GVK key → set of package names that provide it.
	gvkProviders := make(map[string]map[string]bool)
	for _, b := range cfg.Bundles {
		for _, prop := range b.Properties {
			if prop.Type != olmGVK {
				continue
			}
			var g gvkValue
			if json.Unmarshal(prop.Value, &g) != nil {
				continue
			}
			key := g.gvkKey()
			if gvkProviders[key] == nil {
				gvkProviders[key] = make(map[string]bool)
			}
			gvkProviders[key][b.Package] = true
		}
	}

	// Build per-package filter structs from the expanded include list.
	explicitFilters := make(map[string]pkgIncludeFilter, len(allIncludes))
	prevVersionsByPkg := make(map[string]int, len(allIncludes))
	for _, inc := range allIncludes {
		explicitFilters[inc.Name] = buildPkgIncludeFilter(inc)
		prevVersionsByPkg[inc.Name] = inc.PreviousVersions
	}

	// Pre-calculate channel heads for all packages.
	allHeads := make(map[string]map[string][]string) // pkg -> channel -> head bundle names
	allHeadSet := make(map[string]bool)              // bundleName -> isHead
	for pkgName := range catalogPkgs {
		allHeads[pkgName] = make(map[string][]string)
		for _, ch := range channelsByPkg[pkgName] {
			// For transitively discovered packages, we use prev=0 (head only).
			// For explicit packages, we'll use inc.PreviousVersions later.
			heads := channelHeadPlusN(ch, 0)
			allHeads[pkgName][ch.Name] = heads
			for _, h := range heads {
				allHeadSet[h] = true
			}
		}
	}

	// Identify "heads-only" explicit packages: no channels, no version filters.
	// These include the channel head plus PreviousVersions older entries
	// of every channel (oc-mirror v2 behaviour).
	headsOnlyExplicit := make(map[string]bool)
	explicitHeadBundles := make(map[string]bool) // bundleName -> allowed (for BFS and final)
	for pkgName, f := range explicitFilters {
		if !f.allowAllChannels || f.pkgHasMinVer || f.pkgHasMaxVer || len(f.channelFilters) > 0 {
			continue
		}
		// If all channels have empty entries, we cannot determine heads —
		// skip heads-only for this package and include everything.
		allEmpty := true
		for _, ch := range channelsByPkg[pkgName] {
			if len(ch.Entries) > 0 {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			continue
		}
		headsOnlyExplicit[pkgName] = true
		prev := prevVersionsByPkg[pkgName]
		for _, ch := range channelsByPkg[pkgName] {
			selected := channelHeadPlusN(ch, prev)
			for _, s := range selected {
				explicitHeadBundles[s] = true
			}
			fmt.Printf("Package %s channel %s: head+%d → %v\n", pkgName, ch.Name, prev, selected)
		}
	}

	// Resolve transitive dependencies via BFS.
	// explicitHeadBundles tracks allowed bundles for heads-only explicit packages.
	// For other explicit packages, we use their channel/version filters.
	// For transitively discovered packages, we default to heads-only (prev=0).
	allowedBundles := make(map[string]bool)
	allowedChannels := make(map[string]map[string]bool) // pkgName -> channelName set

	queue := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		queue = append(queue, p)
	}

	// We use a separate loop to populate allowedBundles for initial pkgSet,
	// then BFS continues adding to it.
	processedPkgs := make(map[string]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if processedPkgs[current] {
			continue
		}
		processedPkgs[current] = true

		f, isExplicit := explicitFilters[current]

		// Determine allowed bundles for this package.
		var currentAllowed []string

		// Check if the package has any entries in its channels.
		pkgHasEntries := false
		for _, ch := range channelsByPkg[current] {
			if len(ch.Entries) > 0 {
				pkgHasEntries = true
				break
			}
		}

		if isExplicit && !pkgHasEntries {
			// Explicit package but no channel entries found: include all its bundles.
			for _, b := range bundlesByPkg[current] {
				allowedBundles[b.Name] = true
				currentAllowed = append(currentAllowed, b.Name)
			}
		} else if isExplicit && headsOnlyExplicit[current] {
			// Heads-only explicit: use pre-calculated heads.
			for _, ch := range channelsByPkg[current] {
				selected := channelHeadPlusN(ch, prevVersionsByPkg[current])
				for _, h := range selected {
					allowedBundles[h] = true
					currentAllowed = append(currentAllowed, h)
					if allowedChannels[current] == nil {
						allowedChannels[current] = make(map[string]bool)
					}
					allowedChannels[current][ch.Name] = true
				}
			}
		} else if isExplicit {
			// Explicit with channel/version filters.
			for _, ch := range channelsByPkg[current] {
				if !f.allowAllChannels && !f.allowedChannels[ch.Name] {
					continue
				}
				// Liberally mark channel as allowed. Final loop will trim it.
				if allowedChannels[current] == nil {
					allowedChannels[current] = make(map[string]bool)
				}
				allowedChannels[current][ch.Name] = true

				vf := f.effectiveVersionFilter(ch.Name)
				for _, entry := range ch.Entries {
					b, ok := bundlesByName[entry.Name]
					if !ok {
						continue
					}
					if bundleMatchesVersionFilter(b, vf) {
						allowedBundles[entry.Name] = true
						currentAllowed = append(currentAllowed, entry.Name)
					}
				}
			}
		} else if !pkgHasEntries {
			// Transitive dependency with no channel entries: include all its bundles.
			for _, b := range bundlesByPkg[current] {
				allowedBundles[b.Name] = true
				currentAllowed = append(currentAllowed, b.Name)
			}
		} else {
			// Transitive dependency: default to heads-only (prev=0).
			for _, ch := range channelsByPkg[current] {
				heads := allHeads[current][ch.Name]
				for _, h := range heads {
					allowedBundles[h] = true
					currentAllowed = append(currentAllowed, h)
					if allowedChannels[current] == nil {
						allowedChannels[current] = make(map[string]bool)
					}
					allowedChannels[current][ch.Name] = true
				}
			}
		}

		// BFS walk from allowed bundles.
		for _, bName := range currentAllowed {
			b := bundlesByName[bName]
			for _, prop := range b.Properties {
				switch prop.Type {
				case olmPackageRequired:
					var req packageRequiredValue
					if json.Unmarshal(prop.Value, &req) != nil || req.PackageName == "" {
						continue
					}
					if !pkgSet[req.PackageName] && catalogPkgs[req.PackageName] {
						pkgSet[req.PackageName] = true
						queue = append(queue, req.PackageName)
						fmt.Printf("Including dependency package: %s (required by %s via olm.package.required)\n", req.PackageName, current)
					}
				case olmGVKRequired:
					var g gvkValue
					if json.Unmarshal(prop.Value, &g) != nil {
						continue
					}
					for provider := range gvkProviders[g.gvkKey()] {
						if !pkgSet[provider] && catalogPkgs[provider] {
							pkgSet[provider] = true
							queue = append(queue, provider)
							fmt.Printf("Including dependency package: %s (provides %s/%s required by %s)\n", provider, g.Group, g.Kind, current)
						}
					}
				}
			}
		}
	}

	filtered := &declcfg.DeclarativeConfig{
		Packages: make([]declcfg.Package, 0),
		Channels: make([]declcfg.Channel, 0),
		Bundles:  make([]declcfg.Bundle, 0),
	}

	for _, p := range cfg.Packages {
		if pkgSet[p.Name] {
			filtered.Packages = append(filtered.Packages, p)
		}
	}

	for _, ch := range cfg.Channels {
		if !pkgSet[ch.Package] {
			continue
		}
		// Check if this channel was allowed.
		if allowedChannels[ch.Package] == nil || !allowedChannels[ch.Package][ch.Name] {
			continue
		}

		// Trim entries to allowed bundles.
		trimmed := ch
		trimmed.Entries = nil
		for _, e := range ch.Entries {
			if allowedBundles[e.Name] {
				// We keep the original entry structure (Replaces/Skips) but OLM
				// will handle missing references by treating them as new roots.
				// This is better than clearing them because if we included a
				// version range, we want to keep the graph between them.
				trimmed.Entries = append(trimmed.Entries, e)
			}
		}
		if len(trimmed.Entries) > 0 {
			filtered.Channels = append(filtered.Channels, trimmed)
		}
	}

	for _, b := range cfg.Bundles {
		if allowedBundles[b.Name] {
			filtered.Bundles = append(filtered.Bundles, b)
		}
	}

	return filtered, nil
}

// ExtractImages returns all image references found in the FBC
func (r *CatalogResolver) ExtractImages(cfg *declcfg.DeclarativeConfig) []string {
	refs := r.ExtractImagesWithBundles(cfg)
	images := make([]string, 0, len(refs))
	for img := range refs {
		images = append(images, img)
	}
	return images
}

// ExtractImagesWithBundles returns all image references found in the FBC mapped
// to a human-readable string listing the bundle(s) that reference each image.
func (r *CatalogResolver) ExtractImagesWithBundles(cfg *declcfg.DeclarativeConfig) map[string]string {
	// Collect per-image bundle name sets (deduplicated).
	bundleSets := make(map[string]map[string]struct{})
	addRef := func(img, bundleName string) {
		if img == "" {
			return
		}
		if bundleSets[img] == nil {
			bundleSets[img] = make(map[string]struct{})
		}
		bundleSets[img][bundleName] = struct{}{}
	}

	for _, b := range cfg.Bundles {
		addRef(b.Image, b.Name)
		for _, ri := range b.RelatedImages {
			addRef(ri.Image, b.Name)
		}
	}

	result := make(map[string]string, len(bundleSets))
	for img, names := range bundleSets {
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		result[img] = renderBundleRefs(sorted)
	}
	return result
}

// renderBundleRefs formats a sorted, deduped slice of bundle names into a
// compact, human-readable string. Up to 3 names are shown; the rest are
// summarised as "(+N more)".
func renderBundleRefs(names []string) string {
	const maxVisible = 3
	if len(names) <= maxVisible {
		return strings.Join(names, ", ")
	}
	visible := strings.Join(names[:maxVisible], ", ")
	return fmt.Sprintf("%s (+%d more)", visible, len(names)-maxVisible)
}

// ResolveCatalogFull loads the FBC, filters it, and returns both the resulting
// images and the filtered FBC config itself. Used by the manager to persist
// package information in ConfigMaps (Phase 7a).
// The map returned contains destination image reference → bundle-name label.
func (r *CatalogResolver) ResolveCatalogFull(ctx context.Context, catalogImage string, includes []mirrorv1alpha1.IncludePackage) (map[string]string, *declcfg.DeclarativeConfig, error) {
	if _, err := ref.New(catalogImage); err != nil {
		return nil, nil, fmt.Errorf("failed to parse catalog image reference: %w", err)
	}
	if r.client == nil {
		return nil, nil, nil
	}
	cfg, err := r.loadFBCFromImage(ctx, catalogImage)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load FBC from %s: %w", catalogImage, err)
	}
	filtered, err := r.FilterFBC(ctx, cfg, includes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to filter FBC: %w", err)
	}
	return r.ExtractImagesWithBundles(filtered), filtered, nil
}

// ResolveCatalogWithBundles is like ResolveCatalog but returns a map of image
// reference → bundle-name string for use as per-image origin labels.
func (r *CatalogResolver) ResolveCatalogWithBundles(ctx context.Context, catalogImage string, includes []mirrorv1alpha1.IncludePackage) (map[string]string, error) {
	images, _, err := r.ResolveCatalogFull(ctx, catalogImage, includes)
	return images, err
}

// LoadFBC fetches the catalog image layers and parses the File-Based Catalog.
// Returns the full, unfiltered DeclarativeConfig. Use FilterFBC to narrow it.
func (r *CatalogResolver) LoadFBC(ctx context.Context, catalogImage string) (*declcfg.DeclarativeConfig, error) {
	if r.client == nil {
		return nil, fmt.Errorf("registry client is required for LoadFBC")
	}
	return r.loadFBCFromImage(ctx, catalogImage)
}

// BuildFilteredCatalogImage pulls sourceCatalogImage, filters its FBC to the
// requested packages, and pushes a new OCI catalog image to targetRef.
//
// The produced image carries the standard OLM label
//
//	operators.operatorframework.io.index.configs.v1=/configs
//
// so that OLM can serve it directly.  Returns the digest of the pushed manifest.
//
// Internally the source image is first downloaded into a local OCI directory
// layout (via regclient's "ocidir://" scheme) so that all subsequent layer
// reads are local disk I/O — this eliminates redundant network round-trips
// and makes the build resilient to transient registry slowness.
// tlog prints a timestamped log line with wall-clock time, elapsed seconds
// since start, and a step-level duration (since the previous tlog call).
func tlog(start time.Time, prev *time.Time, format string, args ...any) {
	now := time.Now()
	elapsed := now.Sub(start).Seconds()
	step := now.Sub(*prev).Seconds()
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s +%.1fs Δ%.1fs] %s\n", now.Format("15:04:05"), elapsed, step, msg)
	*prev = now
}

func (r *CatalogResolver) BuildFilteredCatalogImage(ctx context.Context, sourceCatalogImage, targetRef string, includes []mirrorv1alpha1.IncludePackage) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("registry client is required for BuildFilteredCatalogImage")
	}

	start := time.Now()
	prev := start
	tlog(start, &prev, "Starting BuildFilteredCatalogImage")

	// 1. Parse destination reference (validated early to fail fast).
	destRef, err := ref.New(targetRef)
	if err != nil {
		return "", fmt.Errorf("failed to parse target reference %s: %w", targetRef, err)
	}

	// 2. Download the source catalog to a local OCI layout.
	// This is a single network pass; regclient handles retries internally.
	tmpDir, err := os.MkdirTemp("", "catalog-build-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for OCI layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tlog(start, &prev, "Downloading source catalog %s to local OCI layout…", sourceCatalogImage)
	if err := r.client.DownloadToOCILayout(ctx, sourceCatalogImage, tmpDir); err != nil {
		return "", fmt.Errorf("failed to download source catalog %s: %w", sourceCatalogImage, err)
	}
	tlog(start, &prev, "Download complete")

	localRef, err := ref.New(fmt.Sprintf("ocidir://%s:source", tmpDir))
	if err != nil {
		return "", fmt.Errorf("failed to build local OCI layout ref: %w", err)
	}

	// 3. Get the local manifest (resolve manifest list → linux/amd64).
	localManifest, err := r.client.ManifestGet(ctx, localRef)
	if err != nil {
		return "", fmt.Errorf("failed to get local manifest for %s: %w", sourceCatalogImage, err)
	}
	localRef, localManifest, err = resolveManifestList(ctx, r.client, localRef, localManifest, sourceCatalogImage)
	if err != nil {
		return "", err
	}

	// 4. Get local image config.
	localConfig, err := r.client.ImageConfig(ctx, localRef)
	if err != nil {
		return "", fmt.Errorf("failed to get local image config: %w", err)
	}

	// 5. Get all source layers from the local layout.
	srcLayers, err := localManifest.GetLayers() //nolint:staticcheck
	if err != nil {
		return "", fmt.Errorf("failed to get source layers: %w", err)
	}
	srcDiffIDs := localConfig.GetConfig().RootFS.DiffIDs
	if len(srcDiffIDs) != len(srcLayers) {
		return "", fmt.Errorf("manifest/config mismatch: %d layers vs %d diff_ids in %s",
			len(srcLayers), len(srcDiffIDs), sourceCatalogImage)
	}
	tlog(start, &prev, "Source catalog %s: %d layers (cached locally)", sourceCatalogImage, len(srcLayers))

	// 6. Classify each layer AND extract FBC content — all reads are local disk I/O.
	cr := classifySourceLayers(ctx, r.client, localRef, srcLayers, srcDiffIDs, sourceCatalogImage)
	tlog(start, &prev, "Layer classification complete: %d kept, %d skipped", len(cr.keptLayers), len(srcLayers)-len(cr.keptLayers))

	// 7. Copy kept layers from local OCI layout to target registry.
	// Reads from disk, uploads to network — each layer transferred only once.
	for i, layer := range cr.keptLayers {
		tlog(start, &prev, "Uploading layer %d/%d (%s, %d bytes)", i+1, len(cr.keptLayers), layer.Digest.String()[:16], layer.Size)
		if err := blobCopyWithRetry(ctx, r.client, localRef, destRef, layer, 3, 5*time.Minute); err != nil {
			return "", fmt.Errorf("failed to upload source layer %d (%s): %w", i, layer.Digest, err)
		}
		tlog(start, &prev, "Layer %d/%d uploaded", i+1, len(cr.keptLayers))
	}

	// 8. Parse the FBC from the already-extracted config files (no re-download).
	if len(cr.configFS) == 0 {
		return "", fmt.Errorf("no FBC config files found under %s in %s", configsPath, sourceCatalogImage)
	}
	subFS, err := fs.Sub(cr.configFS, strings.TrimSuffix(configsPath, "/"))
	if err != nil {
		return "", fmt.Errorf("failed to create configs sub-fs: %w", err)
	}
	cfg, err := declcfg.LoadFS(ctx, subFS)
	if err != nil {
		return "", fmt.Errorf("failed to parse FBC from %s: %w", sourceCatalogImage, err)
	}

	filtered, err := r.FilterFBC(ctx, cfg, includes)
	if err != nil {
		return "", fmt.Errorf("failed to filter FBC: %w", err)
	}
	tlog(start, &prev, "Filtered FBC: %d packages, %d channels, %d bundles",
		len(filtered.Packages), len(filtered.Channels), len(filtered.Bundles))

	// 9. Build the filtered FBC layer (gzip-tar with opaque whiteout).
	tlog(start, &prev, "Building FBC overlay layer…")
	layerData, uncompressedDigest, err := buildFBCLayer(filtered)
	if err != nil {
		return "", fmt.Errorf("failed to build FBC layer: %w", err)
	}
	tlog(start, &prev, "FBC overlay layer built (%d bytes)", len(layerData))

	layerDigest := godigest.FromBytes(layerData)
	layerDesc := descriptor.Descriptor{
		MediaType: mediatype.OCI1LayerGzip,
		Digest:    layerDigest,
		Size:      int64(len(layerData)),
	}

	// 10. Push the filtered FBC layer blob.
	// Use a per-operation timeout to avoid indefinite hangs when the target
	// registry only speaks HTTP and the TLS attempt stalls.
	tlog(start, &prev, "Pushing FBC overlay layer (%d bytes)…", len(layerData))
	pushCtx, pushCancel := context.WithTimeout(ctx, 5*time.Minute)
	_, err = r.client.BlobPut(pushCtx, destRef, layerDesc, bytes.NewReader(layerData))
	pushCancel()
	if err != nil {
		return "", fmt.Errorf("failed to push FBC layer blob: %w", err)
	}
	tlog(start, &prev, "FBC overlay layer pushed")

	// 11. Build new image config: keep source config, replace RootFS.DiffIDs
	// with the kept ones plus our new layer's diff_id.
	imgCfg := localConfig.GetConfig()
	imgCfg.RootFS.DiffIDs = append(append([]godigest.Digest{}, cr.keptDiffIDs...), uncompressedDigest)
	if imgCfg.Config.Labels == nil {
		imgCfg.Config.Labels = make(map[string]string)
	}
	imgCfg.Config.Labels["operators.operatorframework.io.index.configs.v1"] = "/configs"
	imgCfg.Config.Cmd = []string{"serve", "/configs", "--cache-dir=/tmp/cache", "--cache-enforce-integrity=false"}
	now := time.Now().UTC()
	imgCfg.History = append(imgCfg.History, v1.History{
		Created:   &now,
		CreatedBy: "oc-mirror-operator: filtered FBC overlay",
		Comment:   fmt.Sprintf("filtered to %d packages", len(filtered.Packages)),
	})

	newConfig := blob.NewOCIConfig(blob.WithImage(imgCfg))
	configData, err := newConfig.RawBody()
	if err != nil {
		return "", fmt.Errorf("failed to serialize image config: %w", err)
	}
	configDesc := newConfig.GetDescriptor()

	// 12. Push the new config blob.
	tlog(start, &prev, "Pushing image config (%d bytes)…", len(configData))
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 2*time.Minute)
	_, err = r.client.BlobPut(cfgCtx, destRef, configDesc, bytes.NewReader(configData))
	cfgCancel()
	if err != nil {
		return "", fmt.Errorf("failed to push image config blob: %w", err)
	}
	tlog(start, &prev, "Image config pushed")

	// 13. Build manifest: kept source layers + our FBC layer, new config.
	allLayers := make([]descriptor.Descriptor, len(cr.keptLayers)+1)
	copy(allLayers, cr.keptLayers)
	allLayers[len(cr.keptLayers)] = layerDesc

	ociM := v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    configDesc,
		Layers:    allLayers,
	}

	m, err := manifest.New(manifest.WithOrig(ociM))
	if err != nil {
		return "", fmt.Errorf("failed to create OCI manifest: %w", err)
	}

	// 14. Push manifest to target.
	tlog(start, &prev, "Pushing manifest to %s…", targetRef)
	mfCtx, mfCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = r.client.ManifestPut(mfCtx, destRef, m)
	mfCancel()
	if err != nil {
		return "", fmt.Errorf("failed to push catalog manifest to %s: %w", targetRef, err)
	}

	tlog(start, &prev, "DONE — catalog image pushed: %s (kept: %d/%d layers, %d packages)",
		targetRef, len(cr.keptLayers), len(srcLayers), len(filtered.Packages))
	return m.GetDescriptor().Digest.String(), nil
}

// buildFBCLayer serialises a DeclarativeConfig into an in-memory gzip-tar layer
// whose content mirrors the standard catalog image layout:
//
//	configs/<package-name>/catalog.yaml
//
// The layer includes an OCI opaque whiteout (configs/.wh..wh..opq) to ensure
// it completely overrides any /configs content from lower layers.
// It also includes proper directory entries and is deterministically ordered.
//
// Returns the gzip-compressed layer data and the uncompressed tar digest (diff_id).
func buildFBCLayer(cfg *declcfg.DeclarativeConfig) ([]byte, godigest.Digest, error) {
	// Build a per-package map so we can write one catalog.yaml per package.
	pkgCfgs := make(map[string]*declcfg.DeclarativeConfig)
	for _, p := range cfg.Packages {
		pkgCfgs[p.Name] = &declcfg.DeclarativeConfig{
			Packages: []declcfg.Package{p},
		}
	}
	for _, c := range cfg.Channels {
		if pc, ok := pkgCfgs[c.Package]; ok {
			pc.Channels = append(pc.Channels, c)
		}
	}
	for _, b := range cfg.Bundles {
		if pc, ok := pkgCfgs[b.Package]; ok {
			pc.Bundles = append(pc.Bundles, b)
		}
	}

	// Sort package names for deterministic output.
	pkgNames := make([]string, 0, len(pkgCfgs))
	for name := range pkgCfgs {
		pkgNames = append(pkgNames, name)
	}
	sort.Strings(pkgNames)

	// Write uncompressed tar to compute diff_id, then gzip.
	var uncompressedBuf bytes.Buffer
	diffIDHash := sha256.New()
	uncompressedWriter := io.MultiWriter(&uncompressedBuf, diffIDHash)
	tw := tar.NewWriter(uncompressedWriter)

	// Write the configs/ directory entry.
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "configs/",
		Mode:     0755,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to write configs dir header: %w", err)
	}

	// Write OCI opaque whiteout to override all lower-layer /configs content.
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "configs/.wh..wh..opq",
		Size:     0,
		Mode:     0644,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to write opaque whiteout header: %w", err)
	}

	// Invalidate the pre-built opm serve cache from the source catalog image.
	// The source image ships a pogreb cache under /tmp/cache/ that is keyed to
	// the full unfiltered catalog. Our filtered /configs makes it stale, so opm
	// would fatal with "cache requires rebuild". The opaque whiteout removes
	// the old cache directory and opm will rebuild it on first start.
	// We explicitly write tmp/ with mode 01777 (sticky + world-writable) to
	// ensure /tmp is writable in the overlay — opm writes temp files there
	// during cache rebuild (e.g. /tmp/opm-cache-build-*.json).
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "tmp/",
		Mode:     01777,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to write tmp dir header: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "tmp/cache/",
		Mode:     01777,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to write cache dir header: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "tmp/cache/.wh..wh..opq",
		Size:     0,
		Mode:     0644,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to write cache whiteout header: %w", err)
	}

	for _, pkgName := range pkgNames {
		pkgCfg := pkgCfgs[pkgName]

		// Write package directory entry.
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     "configs/" + pkgName + "/",
			Mode:     0755,
		}); err != nil {
			return nil, "", fmt.Errorf("failed to write dir header for %s: %w", pkgName, err)
		}

		var yamlBuf bytes.Buffer
		if err := declcfg.WriteYAML(*pkgCfg, &yamlBuf); err != nil {
			return nil, "", fmt.Errorf("failed to write YAML for package %s: %w", pkgName, err)
		}

		path := "configs/" + pkgName + "/catalog.yaml"
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path,
			Size:     int64(yamlBuf.Len()),
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("failed to write tar header for %s: %w", path, err)
		}
		if _, err := io.Copy(tw, &yamlBuf); err != nil {
			return nil, "", fmt.Errorf("failed to write tar content for %s: %w", path, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to finalise tar: %w", err)
	}

	diffID := godigest.NewDigestFromBytes(godigest.SHA256, diffIDHash.Sum(nil))

	// Gzip the tar.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := io.Copy(gzw, &uncompressedBuf); err != nil {
		return nil, "", fmt.Errorf("failed to gzip tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to finalise gzip: %w", err)
	}

	return gzBuf.Bytes(), diffID, nil
}
