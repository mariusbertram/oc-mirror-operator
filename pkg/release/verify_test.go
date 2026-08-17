package release

import (
	"bytes"
	"crypto"
	"encoding/json"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// nopWriteCloser adapts a bytes.Buffer to io.WriteCloser for
// packet.SerializeLiteral, which needs to Close() the underlying stream.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

// unsignedLiteralMessage builds a bare OpenPGP literal-data packet with no
// surrounding signature — a well-formed message ReadMessage can parse, but
// with IsSigned false.
func unsignedLiteralMessage(content []byte) []byte {
	var buf bytes.Buffer
	pw, err := packet.SerializeLiteral(nopWriteCloser{&buf}, true, "", 0)
	Expect(err).NotTo(HaveOccurred())
	_, err = pw.Write(content)
	Expect(err).NotTo(HaveOccurred())
	Expect(pw.Close()).To(Succeed())
	return buf.Bytes()
}

// testSigningEntity generates a fresh, ephemeral OpenPGP key pair for tests.
// Verifying against the real embedded Red Hat keys would require their
// private key, which we obviously don't have — so tests exercise the same
// verifySignatureAgainst logic against a locally generated keyring instead.
func testSigningEntity() *openpgp.Entity {
	entity, err := openpgp.NewEntity("Test Signer", "", "test@example.com", &packet.Config{
		RSABits: 2048,
	})
	Expect(err).NotTo(HaveOccurred())
	return entity
}

// signPayload produces a signed (non-detached) OpenPGP message wrapping the
// given content, matching the "atomic container signature" format actually
// downloaded from mirror.openshift.com.
func signPayload(entity *openpgp.Entity, content []byte, cfg *packet.Config) []byte {
	var buf bytes.Buffer
	w, err := openpgp.Sign(&buf, entity, nil, cfg)
	Expect(err).NotTo(HaveOccurred())
	_, err = w.Write(content)
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())
	return buf.Bytes()
}

func atomicSignaturePayload(digest string) []byte {
	payload := map[string]interface{}{
		"critical": map[string]interface{}{
			"type":     "atomic container signature",
			"image":    map[string]string{"docker-manifest-digest": digest},
			"identity": map[string]string{"docker-reference": "quay.io/openshift-release-dev/ocp-release"},
		},
		"optional": map[string]interface{}{},
	}
	data, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	return data
}

var _ = Describe("VerifySignature", func() {
	var (
		entity *openpgp.Entity
		digest string
	)

	BeforeEach(func() {
		entity = testSigningEntity()
		digest = "sha256:abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
	})

	It("accepts a valid signature matching the release digest", func() {
		sig := signPayload(entity, atomicSignaturePayload(digest), nil)
		keyring := openpgp.EntityList{entity}
		Expect(verifySignatureAgainst(sig, digest, keyring)).To(Succeed())
	})

	It("rejects a signature whose digest does not match the release digest", func() {
		sig := signPayload(entity, atomicSignaturePayload("sha256:deadbeef"), nil)
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst(sig, digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("does not match release digest"))
	})

	It("rejects a signature signed by an untrusted key", func() {
		sig := signPayload(entity, atomicSignaturePayload(digest), nil)
		otherEntity := testSigningEntity()
		keyring := openpgp.EntityList{otherEntity} // does not contain the signer's key
		err := verifySignatureAgainst(sig, digest, keyring)
		Expect(err).To(HaveOccurred())
	})

	It("rejects malformed OpenPGP data", func() {
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst([]byte("not a pgp message"), digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse signature"))
	})

	It("rejects a signature whose payload is not valid JSON", func() {
		sig := signPayload(entity, []byte("not json"), nil)
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst(sig, digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse signed payload"))
	})

	It("rejects an expired signature", func() {
		// Sign with a 1-second lifetime at the real current time, then wait
		// for it to actually elapse — verification compares against the real
		// wall clock, so backdating the signing time itself would instead
		// make the signer's own key look "not yet valid".
		cfg := &packet.Config{
			DefaultHash:     crypto.SHA256,
			SigLifetimeSecs: 1,
		}
		sig := signPayload(entity, atomicSignaturePayload(digest), cfg)
		time.Sleep(1100 * time.Millisecond)
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst(sig, digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("expired"))
	})

	It("real embedded keyring parses without error", func() {
		// Sanity check that the embedded Red Hat keys (fetched from
		// github.com/openshift/cluster-update-keys) are well-formed and
		// loaded at package init.
		Expect(verificationKeyring).To(HaveLen(2))
	})

	It("rejects an unsigned OpenPGP message", func() {
		msg := unsignedLiteralMessage(atomicSignaturePayload(digest))
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst(msg, digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not a signed OpenPGP message"))
	})

	It("rejects a signature whose content has been tampered with after signing", func() {
		sig := signPayload(entity, atomicSignaturePayload(digest), nil)
		idx := bytes.Index(sig, []byte(digest[len(digest)-8:]))
		Expect(idx).To(BeNumerically(">=", 0), "expected the signed digest to appear as raw (uncompressed) bytes")
		tampered := append([]byte{}, sig...)
		tampered[idx] ^= 0xFF
		keyring := openpgp.EntityList{entity}
		err := verifySignatureAgainst(tampered, digest, keyring)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("signature verification failed"))
	})

	It("uses the real VerifySignature entrypoint against the embedded Red Hat keyring", func() {
		err := VerifySignature([]byte("not a pgp message"), digest)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse signature"))
	})
})
