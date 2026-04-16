package mirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/ref"
)

// MirrorClient handles image mirroring using regclient
type MirrorClient struct {
	rc *regclient.RegClient
}

// NewMirrorClient creates a new MirrorClient
func NewMirrorClient() *MirrorClient {
	return &MirrorClient{
		rc: regclient.New(),
	}
}

// CopyImage copies an image from source to destination, including signatures.
// If the source is a digest-only reference, it creates a tag from the digest in the destination.
func (c *MirrorClient) CopyImage(ctx context.Context, src, dest string) error {
	srcRef, err := ref.New(src)
	if err != nil {
		return fmt.Errorf("failed to parse source reference %s: %w", src, err)
	}

	destRef, err := ref.New(dest)
	if err != nil {
		return fmt.Errorf("failed to parse destination reference %s: %w", dest, err)
	}

	// Handle digest-only images: if srcRef has a digest but no tag,
	// ensure destRef has a tag derived from the digest.
	if srcRef.Digest != "" && srcRef.Tag == "" {
		// Convert digest sha256:abc... to sha256-abc... as a tag
		tag := strings.Replace(srcRef.Digest, ":", "-", 1)
		destRef.Tag = tag
	}

	// Perform the copy with digest tags (signatures) and referrers
	err = c.rc.ImageCopy(ctx, srcRef, destRef,
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

	_, err = c.rc.ManifestHead(ctx, r)
	if err != nil {
		// Check if it's a "not found" error
		// regclient usually returns an error if not found
		return false, nil
	}

	return true, nil
}
