// Package graph builds and pushes the Cincinnati "graph-data" image used by
// the OpenShift Update Service (OSUS) in disconnected environments, matching
// the format oc-mirror v2 produces (registry.access.redhat.com/ubi9/ubi base
// image + a single extra layer containing the graph-data files under
// /var/lib/cincinnati-graph-data, with an entrypoint that copies them into
// the OSUS-mounted /var/lib/cincinnati/graph-data at container start).
//
// See: https://docs.openshift.com/container-platform/4.13/updating/updating-restricted-network-cluster/restricted-network-update-osus.html#update-service-graph-data_updating-restricted-network-cluster-osus
package graph

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// graphDataURL is the upstream Cincinnati endpoint serving a gzip-tar archive
// of the current graph-data (channels, blocked edges, etc.). It is a var (not
// const) so tests can replace it with a local httptest server.
// Network requirement: outbound HTTPS to api.openshift.com:443 (Manager pod).
var graphDataURL = "https://api.openshift.com/api/upgrades_info/graph-data"

const (
	// graphBaseImage is the base image the graph-data layer is appended to,
	// matching oc-mirror v2 exactly.
	graphBaseImage = "registry.access.redhat.com/ubi9/ubi:latest"

	// graphImageRepo is the destination repository path (under the target
	// registry) the graph image is pushed to.
	graphImageRepo = "openshift/graph-image"

	// graphDataDir is the in-container directory the graph-data layer's
	// files are rooted under, matching oc-mirror v2 exactly (including the
	// leading slash baked into the tar entry names).
	graphDataDir = "/var/lib/cincinnati-graph-data"

	// graphDataMountPath is where OSUS expects an init container to have
	// copied the graph data by the time it starts.
	graphDataMountPath = "/var/lib/cincinnati/graph-data"
)

// Builder downloads the Cincinnati graph-data archive and builds/pushes the
// OSUS graph-data image.
type Builder struct {
	client     *mirrorclient.MirrorClient
	httpClient *http.Client
}

func New(client *mirrorclient.MirrorClient) *Builder {
	return &Builder{
		client:     client,
		httpClient: &http.Client{Timeout: 2 * time.Minute},
	}
}

// TargetImage returns the destination reference the graph image is pushed to
// for the given target registry (e.g. "registry.example.com/mirror" →
// "registry.example.com/mirror/openshift/graph-image:latest").
func TargetImage(registry string) string {
	return fmt.Sprintf("%s/%s:latest", registry, graphImageRepo)
}

// BuildAndPush downloads the current Cincinnati graph-data archive, appends
// it as a new layer on top of the UBI9 base image, and pushes the result to
// TargetImage(registry). Returns the pushed manifest digest.
func (b *Builder) BuildAndPush(ctx context.Context, registry string) (string, error) {
	if b.client == nil {
		return "", fmt.Errorf("registry client is required for BuildAndPush")
	}

	archive, err := b.downloadGraphData(ctx)
	if err != nil {
		return "", fmt.Errorf("download graph data: %w", err)
	}

	layerData, diffID, err := buildGraphDataLayer(archive)
	if err != nil {
		return "", fmt.Errorf("build graph data layer: %w", err)
	}

	destRef, err := ref.New(TargetImage(registry))
	if err != nil {
		return "", fmt.Errorf("parse target reference: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "graph-image-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for OCI layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := b.client.DownloadToOCILayout(ctx, graphBaseImage, tmpDir); err != nil {
		return "", fmt.Errorf("download base image %s: %w", graphBaseImage, err)
	}
	localRef, err := ref.New(fmt.Sprintf("ocidir://%s:base", tmpDir))
	if err != nil {
		return "", fmt.Errorf("build local OCI layout ref: %w", err)
	}

	localManifest, err := b.client.ManifestGet(ctx, localRef)
	if err != nil {
		return "", fmt.Errorf("get local base manifest: %w", err)
	}
	localRef, localManifest, err = resolvePlatformManifest(ctx, b.client, localRef, localManifest)
	if err != nil {
		return "", err
	}

	localConfig, err := b.client.ImageConfig(ctx, localRef)
	if err != nil {
		return "", fmt.Errorf("get local base image config: %w", err)
	}

	baseLayers, err := localManifest.GetLayers() //nolint:staticcheck
	if err != nil {
		return "", fmt.Errorf("get base layers: %w", err)
	}
	baseDiffIDs := localConfig.GetConfig().RootFS.DiffIDs
	if len(baseDiffIDs) != len(baseLayers) {
		return "", fmt.Errorf("manifest/config mismatch: %d layers vs %d diff_ids in %s",
			len(baseLayers), len(baseDiffIDs), graphBaseImage)
	}

	for i, layer := range baseLayers {
		if err := copyBlobWithRetry(ctx, b.client, localRef, destRef, layer, 3, 5*time.Minute); err != nil {
			return "", fmt.Errorf("copy base layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	layerDigest := godigest.FromBytes(layerData)
	layerDesc := descriptor.Descriptor{
		MediaType: mediatype.OCI1LayerGzip,
		Digest:    layerDigest,
		Size:      int64(len(layerData)),
	}
	pushCtx, pushCancel := context.WithTimeout(ctx, 5*time.Minute)
	_, err = b.client.BlobPut(pushCtx, destRef, layerDesc, bytes.NewReader(layerData))
	pushCancel()
	if err != nil {
		return "", fmt.Errorf("push graph data layer: %w", err)
	}

	imgCfg := localConfig.GetConfig()
	imgCfg.RootFS.DiffIDs = append(append([]godigest.Digest{}, baseDiffIDs...), diffID)
	imgCfg.Config.Cmd = []string{"/bin/bash", "-c", fmt.Sprintf("exec cp -rp %s/* %s", graphDataDir, graphDataMountPath)}
	now := time.Now().UTC()
	imgCfg.History = append(imgCfg.History, v1.History{
		Created:   &now,
		CreatedBy: "oc-mirror-operator: graph-data image",
		Comment:   "Cincinnati graph-data for OSUS",
	})

	newConfig := blob.NewOCIConfig(blob.WithImage(imgCfg))
	configData, err := newConfig.RawBody()
	if err != nil {
		return "", fmt.Errorf("serialize image config: %w", err)
	}
	configDesc := newConfig.GetDescriptor()

	cfgCtx, cfgCancel := context.WithTimeout(ctx, 2*time.Minute)
	_, err = b.client.BlobPut(cfgCtx, destRef, configDesc, bytes.NewReader(configData))
	cfgCancel()
	if err != nil {
		return "", fmt.Errorf("push image config: %w", err)
	}

	allLayers := make([]descriptor.Descriptor, len(baseLayers)+1)
	copy(allLayers, baseLayers)
	allLayers[len(baseLayers)] = layerDesc

	ociM := v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    configDesc,
		Layers:    allLayers,
	}
	m, err := manifest.New(manifest.WithOrig(ociM))
	if err != nil {
		return "", fmt.Errorf("create OCI manifest: %w", err)
	}

	mfCtx, mfCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = b.client.ManifestPut(mfCtx, destRef, m)
	mfCancel()
	if err != nil {
		return "", fmt.Errorf("push graph image manifest: %w", err)
	}

	return m.GetDescriptor().Digest.String(), nil
}

// downloadGraphData fetches the raw gzip-tar graph-data archive from
// graphDataURL.
func (b *Builder) downloadGraphData(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphDataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", graphDataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", graphDataURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty response body from %s", graphDataURL)
	}
	return data, nil
}

// buildGraphDataLayer re-tars the entries of the downloaded gzip-tar graph-data
// archive with every path rooted under graphDataDir (e.g. "channels/4.14.yaml"
// → "var/lib/cincinnati-graph-data/channels/4.14.yaml"), UID/GID reset to 0,
// and gzips the result. Returns the gzip-compressed layer bytes and the
// uncompressed tar digest (diff_id).
func buildGraphDataLayer(archive []byte) ([]byte, godigest.Digest, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, "", fmt.Errorf("open gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)

	var uncompressedBuf bytes.Buffer
	diffIDHash := godigest.SHA256.Digester()
	uncompressedWriter := io.MultiWriter(&uncompressedBuf, diffIDHash.Hash())
	tw := tar.NewWriter(uncompressedWriter)

	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, "", fmt.Errorf("read graph data tar: %w", nextErr)
		}
		hdr.Name = filepath.Join(graphDataDir, hdr.Name)
		hdr.Uid = 0
		hdr.Gid = 0
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("write tar header for %s: %w", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // graph-data archive size is bounded by downloadGraphData's LimitReader
				return nil, "", fmt.Errorf("write tar content for %s: %w", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize tar: %w", err)
	}

	diffID := diffIDHash.Digest()

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := io.Copy(gzw, &uncompressedBuf); err != nil {
		return nil, "", fmt.Errorf("gzip tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize gzip: %w", err)
	}
	return gzBuf.Bytes(), diffID, nil
}

// resolvePlatformManifest resolves a manifest list to the linux/amd64
// manifest, matching the single-architecture assumption already used
// throughout this codebase's release/catalog resolvers. If the manifest is
// already a single image, it is returned unchanged.
func resolvePlatformManifest(ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, m manifest.Manifest) (ref.Ref, manifest.Manifest, error) {
	if !m.IsList() {
		return r, m, nil
	}
	p, err := platform.Parse("linux/amd64")
	if err != nil {
		return r, nil, fmt.Errorf("parse platform: %w", err)
	}
	desc, err := manifest.GetPlatformDesc(m, &p)
	if err != nil {
		return r, nil, fmt.Errorf("no linux/amd64 manifest in %s: %w", graphBaseImage, err)
	}
	r.Digest = desc.Digest.String()
	r.Tag = ""
	resolved, err := client.ManifestGet(ctx, r)
	if err != nil {
		return r, nil, fmt.Errorf("get platform manifest: %w", err)
	}
	return r, resolved, nil
}

// copyBlobWithRetry transfers a blob from src to dst, retrying transient
// failures. Mirrors catalog.blobCopyWithRetry's approach (explicit
// read-then-write instead of BlobCopy, which may attempt server-side blob
// mounts that hang across different schemes).
func copyBlobWithRetry(ctx context.Context, client *mirrorclient.MirrorClient, src, dst ref.Ref, d descriptor.Descriptor, maxAttempts int, perAttemptTimeout time.Duration) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(attempt-1) * 5 * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		lastErr = copyBlobOnce(attemptCtx, client, src, dst, d)
		cancel()
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

func copyBlobOnce(ctx context.Context, client *mirrorclient.MirrorClient, src, dst ref.Ref, d descriptor.Descriptor) error {
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
