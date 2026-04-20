package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing/fstest"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	godigest "github.com/opencontainers/go-digest"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
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

	queue := make([]string, 0, len(packages))
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

	// 1. Extract and filter the FBC.
	cfg, err := r.loadFBCFromImage(ctx, sourceCatalogImage)
	if err != nil {
		return "", fmt.Errorf("failed to load FBC from %s: %w", sourceCatalogImage, err)
	}

	filtered, err := r.FilterFBC(ctx, cfg, packages)
	if err != nil {
		return "", fmt.Errorf("failed to filter FBC: %w", err)
	}

	// 2. Serialize filtered FBC to YAML and pack into a gzip-tar layer.
	layerData, err := buildFBCLayer(filtered)
	if err != nil {
		return "", fmt.Errorf("failed to build FBC layer: %w", err)
	}

	// 3. Push the layer blob.
	layerDigest := godigest.FromBytes(layerData)
	layerDesc := descriptor.Descriptor{
		MediaType: mediatype.OCI1LayerGzip,
		Digest:    layerDigest,
		Size:      int64(len(layerData)),
	}

	destRef, err := ref.New(targetRef)
	if err != nil {
		return "", fmt.Errorf("failed to parse target reference %s: %w", targetRef, err)
	}

	if _, err = r.client.BlobPut(ctx, destRef, layerDesc, bytes.NewReader(layerData)); err != nil {
		return "", fmt.Errorf("failed to push FBC layer blob: %w", err)
	}

	// 4. Build and push a minimal OCI image config with the OLM catalog label.
	configData, err := buildCatalogImageConfig()
	if err != nil {
		return "", fmt.Errorf("failed to build image config: %w", err)
	}

	configDigest := godigest.FromBytes(configData)
	configDesc := descriptor.Descriptor{
		MediaType: mediatype.OCI1ImageConfig,
		Digest:    configDigest,
		Size:      int64(len(configData)),
	}

	if _, err = r.client.BlobPut(ctx, destRef, configDesc, bytes.NewReader(configData)); err != nil {
		return "", fmt.Errorf("failed to push image config blob: %w", err)
	}

	// 5. Build and push the OCI image manifest.
	ociM := v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    configDesc,
		Layers:    []descriptor.Descriptor{layerDesc},
	}

	m, err := manifest.New(manifest.WithOrig(ociM))
	if err != nil {
		return "", fmt.Errorf("failed to create OCI manifest: %w", err)
	}

	if err = r.client.ManifestPut(ctx, destRef, m); err != nil {
		return "", fmt.Errorf("failed to push catalog manifest to %s: %w", targetRef, err)
	}

	return m.GetDescriptor().Digest.String(), nil
}

// buildFBCLayer serialises a DeclarativeConfig into an in-memory gzip-tar layer
// whose content mirrors the standard catalog image layout:
//
//	configs/<package-name>/catalog.yaml
func buildFBCLayer(cfg *declcfg.DeclarativeConfig) ([]byte, error) {
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

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for pkgName, pkgCfg := range pkgCfgs {
		var yamlBuf bytes.Buffer
		if err := declcfg.WriteYAML(*pkgCfg, &yamlBuf); err != nil {
			return nil, fmt.Errorf("failed to write YAML for package %s: %w", pkgName, err)
		}

		path := "configs/" + pkgName + "/catalog.yaml"
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path,
			Size:     int64(yamlBuf.Len()),
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %s: %w", path, err)
		}
		if _, err := io.Copy(tw, &yamlBuf); err != nil {
			return nil, fmt.Errorf("failed to write tar content for %s: %w", path, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalise tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalise gzip: %w", err)
	}

	return buf.Bytes(), nil
}

// ociImageConfig is a minimal OCI image configuration sufficient for a catalog image.
type ociImageConfig struct {
	Config ociImageConfigInner `json:"config"`
}

type ociImageConfigInner struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

// buildCatalogImageConfig returns a minimal OCI image config JSON that marks
// the image as an OLM FBC catalog (the label tells OLM where configs live).
func buildCatalogImageConfig() ([]byte, error) {
	cfg := ociImageConfig{
		Config: ociImageConfigInner{
			Labels: map[string]string{
				"operators.operatorframework.io.index.configs.v1": "/configs",
			},
		},
	}
	return json.Marshal(cfg)
}
