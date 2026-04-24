package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/errs"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
)

// MirrorClient handles image mirroring using regclient
type MirrorClient struct {
	rc         *regclient.RegClient
	rcFallback *regclient.RegClient // HTTP fallback for insecure hosts
}

// largeBlobThreshold is the size above which blobs are pre-buffered to disk
// before upload. This works around a Quay-specific issue where chunked uploads
// (PATCH-based) lose the upload session before the final PUT, resulting in
// BLOB_UPLOAD_UNKNOWN errors. By buffering to an ephemeral volume first, the
// monolithic PUT streams data quickly from local disk instead of a slow
// cross-registry pipe — without consuming large amounts of memory.
const largeBlobThreshold = 100 * 1024 * 1024 // 100 MiB

// blobBufferDir is the directory used for temporary blob buffer files.
// Worker pods mount an emptyDir volume here.
const blobBufferDir = "/tmp/blob-buffer"

// NewMirrorClient creates a new MirrorClient.
// insecureHosts: registry hostnames where TLS verification is skipped. The
// client first tries HTTPS without certificate validation (TLSInsecure) and
// falls back to plain HTTP (TLSDisabled) if that fails.
// destHosts: registry hostnames of destination registries; configured with BlobMax=-1
// to always use monolithic PUT (fast when blobs are pre-buffered by the reader hook).
// authConfigPath: path to a Docker credential store directory (mounted secret).
func NewMirrorClient(insecureHosts []string, authConfigPath string, destHosts ...string) *MirrorClient {
	opts := []regclient.Opt{}

	hostMap := make(map[string]config.Host)

	// Add destination hosts with BlobMax=-1 (monolithic PUT for all blob sizes).
	// Large blobs are pre-buffered to disk (the worker pod mounts an emptyDir at
	// /tmp/blob-buffer) by the ImageWithBlobReaderHook so the PUT streams fast
	// from local disk, avoiding Quay upload-session expiry.
	for _, h := range destHosts {
		if h == "" {
			continue
		}
		hostMap[h] = config.Host{
			Name:    h,
			BlobMax: -1,
		}
	}
	for _, h := range insecureHosts {
		if h == "" {
			continue
		}
		hostMap[h] = config.Host{
			Name:    h,
			TLS:     config.TLSInsecure,
			BlobMax: -1,
		}
	}

	hostConfigs := make([]config.Host, 0, len(hostMap))
	for _, hc := range hostMap {
		hostConfigs = append(hostConfigs, hc)
	}

	// Add auth config path if provided (e.g. DOCKER_CONFIG or mounted secret)
	if authConfigPath != "" {
		// Try {path}/config.json first (Kubernetes secret mount convention),
		// fall back to the path itself if it looks like a direct config file.
		configFile := authConfigPath + "/config.json"
		opts = append(opts, regclient.WithDockerCredsFile(configFile))
	} else {
		// Fall back to the default Docker credential store ($DOCKER_CONFIG or ~/.docker/config.json)
		opts = append(opts, regclient.WithDockerCreds())
	}

	if len(hostConfigs) > 0 {
		opts = append(opts, regclient.WithConfigHost(hostConfigs...))
	}

	mc := &MirrorClient{
		rc: regclient.New(opts...),
	}

	// Build a fallback client with TLSDisabled (plain HTTP) for insecure hosts.
	// Used when the primary TLSInsecure (HTTPS skip-verify) attempt fails.
	if len(insecureHosts) > 0 {
		fallbackHostMap := make(map[string]config.Host)
		for k, v := range hostMap {
			fallbackHostMap[k] = v
		}
		for _, h := range insecureHosts {
			if h == "" {
				continue
			}
			fallbackHostMap[h] = config.Host{
				Name:    h,
				TLS:     config.TLSDisabled,
				BlobMax: -1,
			}
		}
		fallbackConfigs := make([]config.Host, 0, len(fallbackHostMap))
		for _, hc := range fallbackHostMap {
			fallbackConfigs = append(fallbackConfigs, hc)
		}
		fallbackOpts := []regclient.Opt{}
		if authConfigPath != "" {
			configFile := authConfigPath + "/config.json"
			fallbackOpts = append(fallbackOpts, regclient.WithDockerCredsFile(configFile))
		} else {
			fallbackOpts = append(fallbackOpts, regclient.WithDockerCreds())
		}
		fallbackOpts = append(fallbackOpts, regclient.WithConfigHost(fallbackConfigs...))
		mc.rcFallback = regclient.New(fallbackOpts...)
	}

	return mc
}

// CopyImage copies an image from source to destination, including signatures.
// It returns the effective destination reference that was actually pushed (which may
// differ from dest when src is a digest-only reference and a tag is synthesized).
func (c *MirrorClient) CopyImage(ctx context.Context, src, dest string) (string, error) {
	effectiveDest, err := c.copyImageWith(ctx, c.rc, src, dest)
	if err != nil && c.rcFallback != nil {
		fmt.Printf("HTTPS (skip-verify) failed for %s, falling back to HTTP: %v\n", dest, err)
		return c.copyImageWith(ctx, c.rcFallback, src, dest)
	}
	return effectiveDest, err
}

func (c *MirrorClient) copyImageWith(ctx context.Context, rc *regclient.RegClient, src, dest string) (string, error) {
	srcRef, err := ref.New(src)
	if err != nil {
		return "", fmt.Errorf("failed to parse source reference %s: %w", src, err)
	}

	destRef, err := ref.New(dest)
	if err != nil {
		return "", fmt.Errorf("failed to parse destination reference %s: %w", dest, err)
	}

	// Synthesise a tag from the source digest only when the destination has no explicit tag.
	if srcRef.Digest != "" && srcRef.Tag == "" && destRef.Tag == "" {
		tag := strings.Replace(srcRef.Digest, ":", "-", 1)
		destRef.Tag = tag
	}

	err = rc.ImageCopy(ctx, srcRef, destRef,
		regclient.ImageWithReferrers(),
		regclient.ImageWithBlobReaderHook(bufferLargeBlobs),
	)
	if err != nil {
		return "", fmt.Errorf("failed to copy image %s to %s: %w", src, dest, err)
	}

	effectiveDest := destRef.CommonName()

	// Cosign stores signatures as tag-based manifests: sha256-{digest}.sig
	// These are NOT returned by the OCI referrers API so ImageWithReferrers
	// does not copy them. Copy the .sig tag explicitly when the source has a digest.
	if srcRef.Digest != "" {
		digest := strings.TrimPrefix(srcRef.Digest, "sha256:")
		sigTag := "sha256-" + digest + ".sig"

		srcSigRef := srcRef
		srcSigRef.Tag = sigTag
		srcSigRef.Digest = ""

		destSigRef := destRef
		destSigRef.Tag = sigTag
		destSigRef.Digest = ""

		// Best-effort: skip silently if the .sig tag does not exist at the source.
		if err := rc.ImageCopy(ctx, srcSigRef, destSigRef); err == nil {
			fmt.Printf("Copied cosign signature %s\n", sigTag)
		}
	}

	return effectiveDest, nil
}

// CheckExist checks if an image exists at the destination registry.
// Returns (true, nil) if the image exists, (false, nil) if it does not exist
// (404/MANIFEST_UNKNOWN), or (false, err) for other errors (auth, network).
func (c *MirrorClient) CheckExist(ctx context.Context, image string) (bool, error) {
	exists, err := c.checkExistWith(ctx, c.rc, image)
	if err != nil && c.rcFallback != nil {
		return c.checkExistWith(ctx, c.rcFallback, image)
	}
	return exists, err
}

func (c *MirrorClient) checkExistWith(ctx context.Context, rc *regclient.RegClient, image string) (bool, error) {
	r, err := ref.New(image)
	if err != nil {
		return false, err
	}

	_, err = rc.ManifestHead(ctx, r)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("manifest head for %s: %w", image, err)
	}

	return true, nil
}

// GetDigest returns the digest of an image
func (c *MirrorClient) GetDigest(ctx context.Context, image string) (string, error) {
	digest, err := c.getDigestWith(ctx, c.rc, image)
	if err != nil && c.rcFallback != nil {
		return c.getDigestWith(ctx, c.rcFallback, image)
	}
	return digest, err
}

func (c *MirrorClient) getDigestWith(ctx context.Context, rc *regclient.RegClient, image string) (string, error) {
	r, err := ref.New(image)
	if err != nil {
		return "", err
	}

	m, err := rc.ManifestHead(ctx, r)
	if err != nil {
		return "", err
	}

	return m.GetDescriptor().Digest.String(), nil
}

// ManifestGet retrieves a manifest from the registry.
func (c *MirrorClient) ManifestGet(ctx context.Context, r ref.Ref) (manifest.Manifest, error) {
	return c.rc.ManifestGet(ctx, r)
}

// ManifestPut pushes a manifest to the registry.
func (c *MirrorClient) ManifestPut(ctx context.Context, r ref.Ref, m manifest.Manifest) error {
	return c.rc.ManifestPut(ctx, r, m)
}

// BlobGet retrieves a blob from the registry.
func (c *MirrorClient) BlobGet(ctx context.Context, r ref.Ref, d descriptor.Descriptor) (blob.Reader, error) {
	return c.rc.BlobGet(ctx, r, d)
}

// BlobPut pushes a blob to the registry.
func (c *MirrorClient) BlobPut(ctx context.Context, r ref.Ref, d descriptor.Descriptor, rdr io.Reader) (descriptor.Descriptor, error) {
	return c.rc.BlobPut(ctx, r, d, rdr)
}

// BlobCopy copies a blob from src to dst, using cross-repo mount when possible.
// Large blobs are pre-buffered to disk via the reader hook to avoid upload timeouts.
func (c *MirrorClient) BlobCopy(ctx context.Context, src, dst ref.Ref, d descriptor.Descriptor) error {
	return c.rc.BlobCopy(ctx, src, dst, d, regclient.BlobWithReaderHook(bufferLargeBlobs))
}

// ImageConfig retrieves the OCI image config for a given ref and platform.
func (c *MirrorClient) ImageConfig(ctx context.Context, r ref.Ref) (*blob.BOCIConfig, error) {
	return c.rc.ImageConfig(ctx, r, regclient.ImageWithPlatform("linux/amd64"))
}

// DeleteManifest deletes a manifest (image) from the registry by reference.
// Tag references are resolved to digests before deletion. Returns nil if the
// image was already gone (404).
func (c *MirrorClient) DeleteManifest(ctx context.Context, image string) error {
	err := c.deleteManifestWith(ctx, c.rc, image)
	if err != nil && c.rcFallback != nil {
		return c.deleteManifestWith(ctx, c.rcFallback, image)
	}
	return err
}

func (c *MirrorClient) deleteManifestWith(ctx context.Context, rc *regclient.RegClient, image string) error {
	r, err := ref.New(image)
	if err != nil {
		return fmt.Errorf("failed to parse reference %s: %w", image, err)
	}

	// regclient requires a digest to delete; resolve tag→digest via HEAD.
	if r.Digest == "" {
		m, err := rc.ManifestHead(ctx, r)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return nil // already gone
			}
			return fmt.Errorf("failed to resolve digest for %s: %w", image, err)
		}
		r.Digest = m.GetDescriptor().Digest.String()
	}

	err = rc.ManifestDelete(ctx, r)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil // already gone
		}
		return fmt.Errorf("failed to delete manifest %s: %w", image, err)
	}
	return nil
}

// tempFileReader wraps an os.File and removes the file on Close.
type tempFileReader struct {
	*os.File
}

func (r *tempFileReader) Close() error {
	err := r.File.Close()
	_ = os.Remove(r.Name())
	return err
}

// bufferLargeBlobs is a regclient BlobReaderHook that pre-buffers large blobs
// to disk before they are pushed to the destination.
//
// Without this hook, regclient streams large blobs directly from the source
// registry HTTP response into the destination upload. For very large blobs
// (>100 MiB) this can take minutes, causing Quay to expire the upload session
// before the final PUT completes → BLOB_UPLOAD_UNKNOWN.
//
// By writing the blob to a temp file on an ephemeral volume first, the
// subsequent monolithic PUT (BlobMax=-1) streams from local disk, completing
// quickly without consuming large amounts of memory.
func bufferLargeBlobs(br *blob.BReader) (*blob.BReader, error) {
	d := br.GetDescriptor()
	if d.Size > 0 && d.Size <= largeBlobThreshold {
		return br, nil
	}

	fmt.Printf("Buffering large blob (%d bytes) to disk before upload\n", d.Size)

	f, err := os.CreateTemp(blobBufferDir, "blob-*.tmp")
	if err != nil {
		_ = br.Close()
		return nil, fmt.Errorf("failed to create temp file for blob buffer: %w", err)
	}

	n, err := io.Copy(f, br)
	_ = br.Close()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("failed to buffer blob to disk: %w", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("failed to seek blob buffer file: %w", err)
	}

	if d.Size <= 0 {
		d.Size = n
	}

	return blob.NewReader(
		blob.WithReader(&tempFileReader{f}),
		blob.WithDesc(d),
	), nil
}
