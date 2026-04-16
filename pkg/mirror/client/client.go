package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/ref"
)

// MirrorClient handles image mirroring using regclient
type MirrorClient struct {
	RC *regclient.RegClient
}

// NewMirrorClient creates a new MirrorClient
func NewMirrorClient(insecureHosts ...string) *MirrorClient {
	opts := []regclient.Opt{}
	if len(insecureHosts) > 0 {
		hostConfigs := make([]config.Host, len(insecureHosts))
		for i, h := range insecureHosts {
			hostConfigs[i] = config.Host{
				Name: h,
				TLS:  config.TLSInsecure,
			}
		}
		opts = append(opts, regclient.WithConfigHosts(hostConfigs))
	}
	return &MirrorClient{
		RC: regclient.New(opts...),
	}
}

// CopyImage copies an image from source to destination, including signatures.
func (c *MirrorClient) CopyImage(ctx context.Context, src, dest string) error {
	srcRef, err := ref.New(src)
	if err != nil {
		return fmt.Errorf("failed to parse source reference %s: %w", src, err)
	}

	destRef, err := ref.New(dest)
	if err != nil {
		return fmt.Errorf("failed to parse destination reference %s: %w", dest, err)
	}

	if srcRef.Digest != "" && srcRef.Tag == "" {
		tag := strings.Replace(srcRef.Digest, ":", "-", 1)
		destRef.Tag = tag
	}

	err = c.RC.ImageCopy(ctx, srcRef, destRef,
		regclient.ImageWithDigestTags(),
		regclient.ImageWithReferrers(),
	)
	if err != nil {
		return fmt.Errorf("failed to copy image %s to %s: %w", src, dest, err)
	}

	return nil
}

// CheckExist checks if an image exists at the destination registry
func (c *MirrorClient) CheckExist(ctx context.Context, image string) (bool, error) {
	r, err := ref.New(image)
	if err != nil {
		return false, err
	}

	_, err = c.RC.ManifestHead(ctx, r)
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

	m, err := c.RC.ManifestHead(ctx, r)
	if err != nil {
		return "", err
	}

	return m.GetDescriptor().Digest.String(), nil
}
