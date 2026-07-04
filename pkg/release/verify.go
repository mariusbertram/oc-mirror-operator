package release

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Embedded Red Hat OpenShift release signing keys, fetched verbatim from
// github.com/openshift/cluster-update-keys (keys/verifier-public-key-redhat-release
// and keys/verifier-public-key-redhat-beta-2) — the same keys the
// cluster-version-operator trusts for production and beta release verification.
//
//go:embed keys/redhat-release-key.asc
var redHatReleaseKeyASC string

//go:embed keys/redhat-beta-key.asc
var redHatBetaKeyASC string

// verificationKeyring holds the parsed public keys used to verify OCP/OKD
// release image signatures downloaded from mirror.openshift.com.
var verificationKeyring = mustLoadVerificationKeyring()

func mustLoadVerificationKeyring() openpgp.EntityList {
	var keyring openpgp.EntityList
	for _, armored := range []string{redHatReleaseKeyASC, redHatBetaKeyASC} {
		el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
		if err != nil {
			panic(fmt.Sprintf("release: failed to parse embedded verification key: %v", err))
		}
		keyring = append(keyring, el...)
	}
	return keyring
}

// signaturePayload is the "atomic container signature" JSON format signed by
// the Red Hat release keys. Only the fields needed for verification are
// declared; see https://github.com/containers/image/blob/main/docs/atomic-signature.md.
type signaturePayload struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
	} `json:"critical"`
}

// VerifySignature cryptographically verifies sigData (as downloaded by
// SignatureClient.DownloadSignature) against the embedded Red Hat release
// signing keys, and checks that the signed payload attests to releaseDigest
// (a "sha256:<hex>" manifest digest).
//
// This verifies signature authenticity, integrity, and expiry, and that the
// signed "docker-manifest-digest" matches releaseDigest — enough to detect a
// tampered or substituted release payload. It intentionally does not enforce
// the signature's "docker-reference" identity field: mirrored images are
// legitimately pushed under a destination reference that differs from the
// upstream one, and enforcing identity would require the full
// containers/image signature-policy matching engine, which is out of scope
// here.
func VerifySignature(sigData []byte, releaseDigest string) error {
	return verifySignatureAgainst(sigData, releaseDigest, verificationKeyring)
}

// verifySignatureAgainst is the testable core of VerifySignature, taking an
// explicit keyring so tests can verify against a locally generated key pair
// instead of the real embedded Red Hat keys.
func verifySignatureAgainst(sigData []byte, releaseDigest string, keyring openpgp.EntityList) error {
	md, err := openpgp.ReadMessage(bytes.NewReader(sigData), keyring, nil, nil)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	if !md.IsSigned {
		return fmt.Errorf("signature blob is not a signed OpenPGP message")
	}

	content, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return fmt.Errorf("read signed content: %w", err)
	}
	// The signature check itself only completes once the body has been fully
	// read (see openpgp.MessageDetails), so SignatureError/SignedBy must be
	// inspected after the ReadAll above, not before.
	if md.SignatureError != nil {
		return fmt.Errorf("signature verification failed: %v", md.SignatureError)
	}
	if md.SignedBy == nil {
		return fmt.Errorf("signature not signed by a trusted Red Hat release key (key id %x)", md.SignedByKeyId)
	}
	if md.Signature == nil {
		return fmt.Errorf("signature message carries no signature packet")
	}
	if md.Signature.SigLifetimeSecs != nil {
		expiry := md.Signature.CreationTime.Add(time.Duration(*md.Signature.SigLifetimeSecs) * time.Second)
		if time.Now().After(expiry) {
			return fmt.Errorf("signature expired on %s", expiry)
		}
	}

	var payload signaturePayload
	if err := json.Unmarshal(content, &payload); err != nil {
		return fmt.Errorf("parse signed payload: %w", err)
	}
	if payload.Critical.Image.DockerManifestDigest != releaseDigest {
		return fmt.Errorf("signed digest %q does not match release digest %q",
			payload.Critical.Image.DockerManifestDigest, releaseDigest)
	}
	return nil
}
