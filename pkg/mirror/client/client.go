package client

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
)

// MirrorClient handles image mirroring using regclient
type MirrorClient struct {
	rc *regclient.RegClient
}

// NewMirrorClient creates a new MirrorClient
func NewMirrorClient(insecureHosts []string, authConfigPath string) *MirrorClient {
	opts := []regclient.Opt{}

	hostConfigs := make([]config.Host, 0)
	if len(insecureHosts) > 0 {
		for _, h := range insecureHosts {
			hostConfigs = append(hostConfigs, config.Host{
				Name: h,
				TLS:  config.TLSDisabled,
			})
		}
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
		opts = append(opts, regclient.WithConfigHosts(hostConfigs))
	}

	return &MirrorClient{
		rc: regclient.New(opts...),
	}
}

// CopyImage copies an image from source to destination, including signatures.
// It returns the effective destination reference that was actually pushed (which may
// differ from dest when src is a digest-only reference and a tag is synthesized).
func (c *MirrorClient) CopyImage(ctx context.Context, src, dest string) (string, error) {
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

	err = c.rc.ImageCopy(ctx, srcRef, destRef)
	if err != nil {
		return "", fmt.Errorf("failed to copy image %s to %s: %w", src, dest, err)
	}

	return destRef.CommonName(), nil
}

// CheckExist checks if an image exists at the destination registry
func (c *MirrorClient) CheckExist(ctx context.Context, image string) (bool, error) {
	r, err := ref.New(image)
	if err != nil {
		return false, err
	}

	_, err = c.rc.ManifestHead(ctx, r)
	if err != nil {
		return false, nil
	}

	return true, nil
}

// GetDigest returns the digest of an image
func (c *MirrorClient) GetDigest(ctx context.Context, image string) (string, error) {
	r, err := ref.New(image)
	if err != nil {
		return "", err
	}

	m, err := c.rc.ManifestHead(ctx, r)
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
