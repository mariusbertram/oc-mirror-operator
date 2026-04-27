package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotImplemented is returned when a feature is recognised but the
// implementation is incomplete.
var ErrNotImplemented = errors.New("not implemented")

// SignatureClient downloads OpenShift release signatures from the official
// mirror.
type SignatureClient struct {
	httpClient *http.Client
}

func NewSignatureClient() *SignatureClient {
	return &SignatureClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// DownloadSignature fetches the raw GPG signature for a given release digest.
// The digest must be in the form "sha256:hash".
// URL pattern: https://mirror.openshift.com/pub/openshift-v4/signatures/openshift/release/sha256=hash/signature-1
func (c *SignatureClient) DownloadSignature(ctx context.Context, releaseDigest string) ([]byte, error) {
	if !strings.HasPrefix(releaseDigest, "sha256:") {
		return nil, fmt.Errorf("invalid digest format, expected sha256:hash, got %q", releaseDigest)
	}
	hash := strings.TrimPrefix(releaseDigest, "sha256:")
	url := fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/signatures/openshift/release/sha256=%s/signature-1", hash)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("signature not found for digest %s", releaseDigest)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download signature: HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// ReplicateSignature is a no-op stub for backward compatibility with tests.
func (c *SignatureClient) ReplicateSignature(_ context.Context, _, _ string) error {
	return ErrNotImplemented
}
