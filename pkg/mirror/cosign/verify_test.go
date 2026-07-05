package cosign

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

const testImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func generateTestKeyPair(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes, err := cryptoutils.MarshalPublicKeyToPEM(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return priv, pemBytes
}

func signPayload(t *testing.T, priv *ecdsa.PrivateKey, payload []byte) []byte {
	t.Helper()
	signer, err := signature.LoadECDSASigner(priv, crypto.SHA256)
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}
	sig, err := signer.SignMessage(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}
	return sig
}

// buildPayload builds a "simple signing" payload attesting to digest.
func buildPayload(t *testing.T, digest string) []byte {
	t.Helper()
	p := map[string]interface{}{
		"critical": map[string]interface{}{
			"identity": map[string]interface{}{"docker-reference": "example/repo"},
			"image":    map[string]interface{}{"docker-manifest-digest": digest},
			"type":     "cosign container image signature",
		},
		"optional": map[string]interface{}{},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// signatureLayer describes a single layer to serve in the fake signature
// manifest.
type signatureLayer struct {
	payload    []byte
	sigB64     string
	noAnnotate bool // if true, omit the signature annotation entirely
}

// fakeSignatureRegistry serves a cosign signature manifest at
// <repo>:sha256-<hex(imageDigest)>.sig containing the given layers, and
// returns the registry host (no scheme).
//
//nolint:unparam
func fakeSignatureRegistry(t *testing.T, imageDigest string, layers []signatureLayer) string {
	t.Helper()

	configJSON := []byte(`{}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(configJSON))

	blobs := map[string][]byte{configDigest: configJSON}
	layerDescs := make([]map[string]interface{}, 0, len(layers))
	for _, l := range layers {
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(l.payload))
		blobs[digest] = l.payload
		desc := map[string]interface{}{
			"mediaType": "application/vnd.dev.cosign.simplesigning.v1+json",
			"digest":    digest,
			"size":      len(l.payload),
		}
		if !l.noAnnotate {
			desc["annotations"] = map[string]string{signatureAnnotationKey: l.sigB64}
		}
		layerDescs = append(layerDescs, desc)
	}

	manifestJSON, err := json.Marshal(map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configJSON),
		},
		"layers": layerDescs,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestJSON))
	sigTag := "sha256-" + strings.TrimPrefix(imageDigest, "sha256:") + ".sig"

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v2/" || path == "/v2":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(path, "/manifests/"+sigTag):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestJSON)
		case strings.Contains(path, "/manifests/"):
			http.NotFound(w, r)
		case strings.Contains(path, "/blobs/"):
			for digest, content := range blobs {
				if strings.Contains(path, digest) {
					_, _ = w.Write(content)
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestVerifyImageSignature_Valid(t *testing.T) {
	priv, pubPEM := generateTestKeyPair(t)
	payload := buildPayload(t, testImageDigest)
	sigB64 := base64.StdEncoding.EncodeToString(signPayload(t, priv, payload))

	host := fakeSignatureRegistry(t, testImageDigest, []signatureLayer{{payload: payload, sigB64: sigB64}})
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err != nil {
		t.Fatalf("expected valid signature to verify, got: %v", err)
	}
}

func TestVerifyImageSignature_WrongKey(t *testing.T) {
	priv, _ := generateTestKeyPair(t)
	_, wrongPubPEM := generateTestKeyPair(t)
	payload := buildPayload(t, testImageDigest)
	sigB64 := base64.StdEncoding.EncodeToString(signPayload(t, priv, payload))

	host := fakeSignatureRegistry(t, testImageDigest, []signatureLayer{{payload: payload, sigB64: sigB64}})
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, wrongPubPEM)
	if err == nil {
		t.Fatal("expected verification error for signature made with a different key")
	}
}

func TestVerifyImageSignature_TamperedPayloadDigest(t *testing.T) {
	priv, pubPEM := generateTestKeyPair(t)
	otherDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	// Payload is validly signed, but attests to a *different* image digest
	// than the one we're asking to verify — must be rejected even though
	// the cryptographic signature itself is valid.
	payload := buildPayload(t, otherDigest)
	sigB64 := base64.StdEncoding.EncodeToString(signPayload(t, priv, payload))

	host := fakeSignatureRegistry(t, testImageDigest, []signatureLayer{{payload: payload, sigB64: sigB64}})
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error when signed payload digest does not match requested digest")
	}
}

func TestVerifyImageSignature_TamperedSignatureBytes(t *testing.T) {
	priv, pubPEM := generateTestKeyPair(t)
	payload := buildPayload(t, testImageDigest)
	sig := signPayload(t, priv, payload)
	sig[len(sig)-1] ^= 0xFF // flip a bit
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	host := fakeSignatureRegistry(t, testImageDigest, []signatureLayer{{payload: payload, sigB64: sigB64}})
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error for a tampered signature")
	}
}

func TestVerifyImageSignature_MissingAnnotation(t *testing.T) {
	_, pubPEM := generateTestKeyPair(t)
	payload := buildPayload(t, testImageDigest)

	host := fakeSignatureRegistry(t, testImageDigest, []signatureLayer{{payload: payload, noAnnotate: true}})
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error when the signature layer has no signature annotation")
	}
}

func TestVerifyImageSignature_NoLayers(t *testing.T) {
	_, pubPEM := generateTestKeyPair(t)

	host := fakeSignatureRegistry(t, testImageDigest, nil)
	client := mirrorclient.NewMirrorClient([]string{host}, "")

	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error when the signature manifest has no layers")
	}
}

func TestVerifyImageSignature_NoSignatureManifest(t *testing.T) {
	_, pubPEM := generateTestKeyPair(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	client := mirrorclient.NewMirrorClient([]string{host}, "")
	err := VerifyImageSignature(context.Background(), client, host+"/example/repo:v1", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error when no signature manifest exists")
	}
}

func TestVerifyImageSignature_InvalidPublicKey(t *testing.T) {
	client := mirrorclient.NewMirrorClient(nil, "")
	err := VerifyImageSignature(context.Background(), client, "registry.example.com/example/repo:v1", testImageDigest, []byte("not a pem key"))
	if err == nil {
		t.Fatal("expected error for an invalid public key")
	}
}

func TestVerifyImageSignature_InvalidImageDigest(t *testing.T) {
	_, pubPEM := generateTestKeyPair(t)
	client := mirrorclient.NewMirrorClient(nil, "")
	err := VerifyImageSignature(context.Background(), client, "registry.example.com/example/repo:v1", "not-a-digest", pubPEM)
	if err == nil {
		t.Fatal("expected error for a non-sha256 image digest")
	}
}

func TestVerifyImageSignature_InvalidImageRef(t *testing.T) {
	_, pubPEM := generateTestKeyPair(t)
	client := mirrorclient.NewMirrorClient(nil, "")
	err := VerifyImageSignature(context.Background(), client, ":::invalid", testImageDigest, pubPEM)
	if err == nil {
		t.Fatal("expected error for an invalid image reference")
	}
}
