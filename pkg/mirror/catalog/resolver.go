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
	"sort"
	"strings"
	"testing/fstest"
	"time"

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

type CatalogResolver struct {
	client *mirrorclient.MirrorClient
}

func New(client *mirrorclient.MirrorClient) *CatalogResolver {
	return &CatalogResolver{client: client}
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

	cfg, err := r.loadFBCFromImage(ctx, catalogImage)
	if err != nil {
		return nil, fmt.Errorf("failed to load FBC from %s: %w", catalogImage, err)
	}

	filtered, err := r.FilterFBC(ctx, cfg, packages)
	if err != nil {
		return nil, fmt.Errorf("failed to filter FBC: %w", err)
	}

	fmt.Printf("Catalog %s: filtered to %d packages, %d channels, %d bundles\n",
		catalogImage, len(filtered.Packages), len(filtered.Channels), len(filtered.Bundles))

	return r.ExtractImages(filtered), nil
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

	layers, err := m.GetLayers()
	if err != nil {
		return nil, fmt.Errorf("failed to get layers: %w", err)
	}

	// Build an in-memory overlay FS from all layers.
	// Later layers override earlier ones (standard OCI overlay semantics).
	configFS := make(fstest.MapFS)
	for _, layer := range layers {
		blobRdr, blobErr := r.client.BlobGet(ctx, imgRef, layer)
		if blobErr != nil {
			continue // non-fatal: skip unreadable layers
		}
		_ = extractFBCLayer(blobRdr, configFS)
		_ = blobRdr.Close()
	}

	if len(configFS) == 0 {
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
	defer gz.Close()

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

		data, readErr := io.ReadAll(tr)
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

// FilterFBC implements the in-memory filtering of a declarative configuration.
// It includes transitive dependencies by resolving both olm.package.required
// and olm.gvk.required properties from bundles of selected packages.
func (r *CatalogResolver) FilterFBC(ctx context.Context, cfg *declcfg.DeclarativeConfig, packages []string) (*declcfg.DeclarativeConfig, error) {
	if len(packages) == 0 {
		return cfg, nil
	}

	// Build index of which packages exist in the full catalog.
	catalogPkgs := make(map[string]bool, len(cfg.Packages))
	for _, p := range cfg.Packages {
		catalogPkgs[p.Name] = true
	}

	bundlesByPkg := make(map[string][]declcfg.Bundle)
	for _, b := range cfg.Bundles {
		bundlesByPkg[b.Package] = append(bundlesByPkg[b.Package], b)
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

	// Resolve transitive dependencies via BFS.
	pkgSet := make(map[string]bool, len(packages))
	for _, p := range packages {
		pkgSet[p] = true
	}

	// Auto-discover companion dependency packages (Red Hat convention).
	// For "foo-operator" check "foo-dependencies"; also check
	// "<name>-dependencies", "<name>-dependency", "<name>-deps".
	for _, p := range packages {
		candidates := []string{p + "-dependencies", p + "-dependency", p + "-deps"}
		if strings.HasSuffix(p, "-operator") {
			base := strings.TrimSuffix(p, "-operator")
			candidates = append(candidates, base+"-dependencies")
		}
		for _, c := range candidates {
			if catalogPkgs[c] && !pkgSet[c] {
				pkgSet[c] = true
				fmt.Printf("Including companion dependency package: %s (for %s)\n", c, p)
				break // one match is enough
			}
		}
	}

	queue := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		queue = append(queue, p)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, b := range bundlesByPkg[current] {
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
	for _, c := range cfg.Channels {
		if pkgSet[c.Package] {
			filtered.Channels = append(filtered.Channels, c)
		}
	}
	for _, b := range cfg.Bundles {
		if pkgSet[b.Package] {
			filtered.Bundles = append(filtered.Bundles, b)
		}
	}

	return filtered, nil
}

// ExtractImages returns all image references found in the FBC
func (r *CatalogResolver) ExtractImages(cfg *declcfg.DeclarativeConfig) []string {
	imageMap := make(map[string]bool)

	for _, b := range cfg.Bundles {
		if b.Image != "" {
			imageMap[b.Image] = true
		}
		for _, ri := range b.RelatedImages {
			if ri.Image != "" {
				imageMap[ri.Image] = true
			}
		}
	}

	var images []string
	for img := range imageMap {
		images = append(images, img)
	}
	return images
}

// BuildFilteredCatalogImage pulls sourceCatalogImage, filters its FBC to the
// requested packages, and pushes a new OCI catalog image to targetRef.
//
// The produced image carries the standard OLM label
//
//	operators.operatorframework.io.index.configs.v1=/configs
//
// so that OLM can serve it directly.  Returns the digest of the pushed manifest.
func (r *CatalogResolver) BuildFilteredCatalogImage(ctx context.Context, sourceCatalogImage, targetRef string, packages []string) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("registry client is required for BuildFilteredCatalogImage")
	}

	// 1. Parse references.
	srcRef, err := ref.New(sourceCatalogImage)
	if err != nil {
		return "", fmt.Errorf("failed to parse source catalog reference %s: %w", sourceCatalogImage, err)
	}
	destRef, err := ref.New(targetRef)
	if err != nil {
		return "", fmt.Errorf("failed to parse target reference %s: %w", targetRef, err)
	}

	// 2. Get the source manifest (resolve manifest list → linux/amd64).
	srcManifest, err := r.client.ManifestGet(ctx, srcRef)
	if err != nil {
		return "", fmt.Errorf("failed to get source manifest for %s: %w", sourceCatalogImage, err)
	}
	if srcManifest.IsList() {
		p, parseErr := platform.Parse("linux/amd64")
		if parseErr != nil {
			return "", fmt.Errorf("failed to parse platform: %w", parseErr)
		}
		desc, descErr := manifest.GetPlatformDesc(srcManifest, &p)
		if descErr != nil {
			return "", fmt.Errorf("no linux/amd64 manifest in %s: %w", sourceCatalogImage, descErr)
		}
		srcRef.Digest = desc.Digest.String()
		srcRef.Tag = ""
		srcManifest, err = r.client.ManifestGet(ctx, srcRef)
		if err != nil {
			return "", fmt.Errorf("failed to get platform manifest: %w", err)
		}
	}

	// 3. Get source image config (preserves entrypoint, labels, rootfs, etc.).
	srcConfig, err := r.client.ImageConfig(ctx, srcRef)
	if err != nil {
		return "", fmt.Errorf("failed to get source image config: %w", err)
	}

	// 4. Get all source layers.
	srcLayers, err := srcManifest.GetLayers()
	if err != nil {
		return "", fmt.Errorf("failed to get source layers: %w", err)
	}
	fmt.Printf("Source catalog %s: %d layers\n", sourceCatalogImage, len(srcLayers))

	// 5. Copy ALL source layers to target (cross-repo mount if same registry).
	for i, layer := range srcLayers {
		fmt.Printf("Copying source layer %d/%d (%s, %d bytes)\n", i+1, len(srcLayers), layer.Digest.String()[:16], layer.Size)
		if err := r.client.BlobCopy(ctx, srcRef, destRef, layer); err != nil {
			return "", fmt.Errorf("failed to copy source layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	// 6. Extract and filter the FBC.
	cfg, err := r.loadFBCFromImage(ctx, sourceCatalogImage)
	if err != nil {
		return "", fmt.Errorf("failed to load FBC from %s: %w", sourceCatalogImage, err)
	}

	filtered, err := r.FilterFBC(ctx, cfg, packages)
	if err != nil {
		return "", fmt.Errorf("failed to filter FBC: %w", err)
	}
	fmt.Printf("Filtered FBC: %d packages, %d channels, %d bundles\n",
		len(filtered.Packages), len(filtered.Channels), len(filtered.Bundles))

	// 7. Build the filtered FBC layer (gzip-tar with opaque whiteout).
	layerData, uncompressedDigest, err := buildFBCLayer(filtered)
	if err != nil {
		return "", fmt.Errorf("failed to build FBC layer: %w", err)
	}

	layerDigest := godigest.FromBytes(layerData)
	layerDesc := descriptor.Descriptor{
		MediaType: mediatype.OCI1LayerGzip,
		Digest:    layerDigest,
		Size:      int64(len(layerData)),
	}

	// 8. Push the filtered FBC layer blob.
	if _, err = r.client.BlobPut(ctx, destRef, layerDesc, bytes.NewReader(layerData)); err != nil {
		return "", fmt.Errorf("failed to push FBC layer blob: %w", err)
	}

	// 9. Build new image config: keep source config, append our diff_id.
	imgCfg := srcConfig.GetConfig()
	imgCfg.RootFS.DiffIDs = append(imgCfg.RootFS.DiffIDs, uncompressedDigest)
	// Ensure the OLM catalog label is set.
	if imgCfg.Config.Labels == nil {
		imgCfg.Config.Labels = make(map[string]string)
	}
	imgCfg.Config.Labels["operators.operatorframework.io.index.configs.v1"] = "/configs"
	// Since we remove the pre-built cache (opaque whiteout), disable integrity
	// enforcement so opm rebuilds it on first serve.
	imgCfg.Config.Cmd = []string{"serve", "/configs", "--cache-dir=/tmp/cache", "--cache-enforce-integrity=false"}
	// Append a history entry for our layer.
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

	// 10. Push the new config blob.
	if _, err = r.client.BlobPut(ctx, destRef, configDesc, bytes.NewReader(configData)); err != nil {
		return "", fmt.Errorf("failed to push image config blob: %w", err)
	}

	// 11. Build manifest: all source layers + our FBC layer, new config.
	allLayers := make([]descriptor.Descriptor, len(srcLayers)+1)
	copy(allLayers, srcLayers)
	allLayers[len(srcLayers)] = layerDesc

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

	// 12. Push manifest to target.
	if err = r.client.ManifestPut(ctx, destRef, m); err != nil {
		return "", fmt.Errorf("failed to push catalog manifest to %s: %w", targetRef, err)
	}

	fmt.Printf("Catalog image pushed: %s (source layers: %d, filtered packages: %d)\n",
		targetRef, len(srcLayers), len(filtered.Packages))
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


