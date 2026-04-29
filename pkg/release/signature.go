package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/regclient/regclient"
)

// ErrNotImplemented is returned when a feature is recognised but the
// implementation is incomplete. Callers should inspect this sentinel via
// errors.Is to skip the feature gracefully without surfacing it as a real
// error.
var ErrNotImplemented = errors.New("not implemented")

// signatureBaseURL is the upstream source for OpenShift release GPG signatures.
const signatureBaseURL = "https://mirror.openshift.com/pub/openshift-v4/signatures/openshift/release"

// SignatureClient downloads OpenShift release GPG signatures from the upstream
// Red Hat mirror and can replicate them into a target registry.
type SignatureClient struct {
	rc         *regclient.RegClient
	httpClient *http.Client
}

func NewSignatureClient(rc *regclient.RegClient) *SignatureClient {
	return &SignatureClient{
		rc: rc,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// DownloadSignature fetches the GPG signature for a release image digest from
// the Red Hat mirror. The digest must be in the form "sha256:<hex>".
// The URL pattern is:
//
//	https://mirror.openshift.com/pub/openshift-v4/signatures/openshift/release/sha256=<hex>/signature-1
//
// Returns the raw signature bytes on success or an error if the signature is
// not found or the download fails.
func (c *SignatureClient) DownloadSignature(ctx context.Context, releaseDigest string) ([]byte, error) {
	// Convert "sha256:<hex>" → "sha256=<hex>" for the URL path component.
	urlDigest := strings.ReplaceAll(releaseDigest, ":", "=")
	url := fmt.Sprintf("%s/%s/signature-1", signatureBaseURL, urlDigest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build signature request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download signature for %s: %w", releaseDigest, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("signature not found for %s (HTTP 404)", releaseDigest)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download signature for %s: HTTP %d", releaseDigest, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read signature body for %s: %w", releaseDigest, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty signature body for %s", releaseDigest)
	}
	return data, nil
}

// ReplicateSignature is a no-op stub that returns ErrNotImplemented. The
// previous implementation downloaded a signature blob but never persisted it,
// silently swallowing the failure. Returning a sentinel error makes the
// missing functionality visible to callers and tests.
func (c *SignatureClient) ReplicateSignature(_ context.Context, _, _ string) error {
	return ErrNotImplemented
}
