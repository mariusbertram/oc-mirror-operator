// Package cosign provides optional cosign/sigstore public-key signature
// verification for container images. Unlike OpenShift release payloads
// (pkg/release), which are always verified against embedded Red Hat keys,
// there is no single trusted signer for third-party operator catalogs — so
// verification here is opt-in per catalog, with the public key supplied by
// the caller (typically loaded from a Kubernetes Secret referenced in the
// ImageSet spec).
package cosign

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// signatureAnnotationKey is the OCI layer annotation cosign attaches the
// base64-encoded signature under. This has been stable since cosign's
// initial release and is unrelated to the (optional) keyless/Rekor bundle
// annotations, so plain public-key verification only needs this one.
const signatureAnnotationKey = "dev.cosignproject.cosign/signature"

// simpleSigningPayload is the "simple signing" payload format cosign signs
// for container image signatures, matching
// https://github.com/containers/image/blob/main/docs/atomic-signature.md —
// only the field needed to cross-check the signed digest is declared.
type simpleSigningPayload struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// VerifyImageSignature fetches the cosign signature attached to the image at
// imageRef (whose manifest digest is imageDigest, a "sha256:<hex>" string)
// and verifies it against publicKeyPEM (a PEM-encoded public key, as
// produced by "cosign generate-key-pair"). Returns nil if at least one
// attached signature is valid and attests to imageDigest; otherwise returns
// an error describing why no signature could be verified.
func VerifyImageSignature(ctx context.Context, client *mirrorclient.MirrorClient, imageRef, imageDigest string, publicKeyPEM []byte) error {
	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(publicKeyPEM)
	if err != nil {
		return fmt.Errorf("parse cosign public key: %w", err)
	}
	verifier, err := signature.LoadVerifier(pubKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("load verifier for public key: %w", err)
	}

	sigRef, err := signatureRef(imageRef, imageDigest)
	if err != nil {
		return fmt.Errorf("build signature reference: %w", err)
	}

	m, err := client.ManifestGet(ctx, sigRef)
	if err != nil {
		return fmt.Errorf("fetch signature manifest %s: %w", sigRef.CommonName(), err)
	}
	layers, err := m.GetLayers() //nolint:staticcheck
	if err != nil {
		return fmt.Errorf("get signature layers: %w", err)
	}
	if len(layers) == 0 {
		return fmt.Errorf("no signatures found at %s", sigRef.CommonName())
	}

	var lastErr error
	for _, layer := range layers {
		if err := verifyLayerSignature(ctx, client, sigRef, layer, verifier, imageDigest); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("no valid signature found at %s: %w", sigRef.CommonName(), lastErr)
}

// verifyLayerSignature checks a single signature layer: its annotation must
// carry a base64 signature that verifies against the layer's own blob
// content (the signed payload), and that payload must attest to imageDigest.
func verifyLayerSignature(ctx context.Context, client *mirrorclient.MirrorClient, sigRef ref.Ref, layer descriptor.Descriptor, verifier signature.Verifier, imageDigest string) error {
	sigB64, ok := layer.Annotations[signatureAnnotationKey]
	if !ok || sigB64 == "" {
		return fmt.Errorf("signature layer missing %s annotation", signatureAnnotationKey)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	rdr, err := client.BlobGet(ctx, sigRef, layer)
	if err != nil {
		return fmt.Errorf("fetch signature payload: %w", err)
	}
	payload, err := io.ReadAll(rdr)
	_ = rdr.Close()
	if err != nil {
		return fmt.Errorf("read signature payload: %w", err)
	}

	var sp simpleSigningPayload
	if err := json.Unmarshal(payload, &sp); err != nil {
		return fmt.Errorf("parse signature payload: %w", err)
	}
	if sp.Critical.Image.DockerManifestDigest != imageDigest {
		return fmt.Errorf("signature payload digest %q does not match expected %q",
			sp.Critical.Image.DockerManifestDigest, imageDigest)
	}

	if err := verifier.VerifySignature(bytes.NewReader(sigBytes), bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}

// signatureRef builds the cosign signature tag reference for an image
// digest, e.g. imageRef "registry.io/ns/repo:v1" with imageDigest
// "sha256:abcd..." resolves to "registry.io/ns/repo:sha256-abcd....sig",
// matching cosign's fixed tag-based signature storage convention.
func signatureRef(imageRef, imageDigest string) (ref.Ref, error) {
	r, err := ref.New(imageRef)
	if err != nil {
		return ref.Ref{}, fmt.Errorf("parse image reference %s: %w", imageRef, err)
	}
	hex := strings.TrimPrefix(imageDigest, "sha256:")
	if hex == "" || hex == imageDigest {
		return ref.Ref{}, fmt.Errorf("image digest %q is not a sha256 digest", imageDigest)
	}
	r.Tag = "sha256-" + hex + ".sig"
	r.Digest = ""
	return r, nil
}
