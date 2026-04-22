package release

import (
	"context"
	"errors"

	"github.com/regclient/regclient"
)

// ErrNotImplemented is returned when a feature is recognised but the
// implementation is incomplete. Callers should inspect this sentinel via
// errors.Is to skip the feature gracefully without surfacing it as a real
// error.
var ErrNotImplemented = errors.New("not implemented")

// SignatureClient is a placeholder for OpenShift release signature replication.
// The actual push-to-registry path requires assembling an OCI image manifest
// around the signature blob; that work is tracked separately. Until then, the
// client returns ErrNotImplemented so callers can detect the unsupported state.
type SignatureClient struct {
	rc *regclient.RegClient
}

func NewSignatureClient(rc *regclient.RegClient) *SignatureClient {
	return &SignatureClient{rc: rc}
}

// ReplicateSignature is a no-op stub that returns ErrNotImplemented. The
// previous implementation downloaded a signature blob but never persisted it,
// silently swallowing the failure. Returning a sentinel error makes the
// missing functionality visible to callers and tests.
func (c *SignatureClient) ReplicateSignature(_ context.Context, _, _ string) error {
	return ErrNotImplemented
}
