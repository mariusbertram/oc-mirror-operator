package manager

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

	"github.com/sigstore/sigstore/pkg/signature"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifyOperatorCatalogSignature_SecretNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-secret"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error when the public key Secret does not exist")
	}
	if !strings.Contains(err.Error(), "get public key secret") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestVerifyOperatorCatalogSignature_SecretMissingKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cosign-key", Namespace: "test-ns"},
		Data:       map[string][]byte{"other-key": []byte("irrelevant")},
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cosign-key"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error when the Secret has no data for the referenced key")
	}
	if !strings.Contains(err.Error(), `no key "cosign.pub"`) {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestVerifyOperatorCatalogSignature_InvalidPublicKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cosign-key", Namespace: "test-ns"},
		Data:       map[string][]byte{"cosign.pub": []byte("not a pem key")},
	}
	m := &MirrorManager{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Namespace:  "test-ns",
		TargetName: "test-target",
	}
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{Registry: "registry.example.com/mirror"}}
	op := mirrorv1alpha1.Operator{
		Catalog: "registry.example.com/catalog:v1",
		SignatureVerification: &mirrorv1alpha1.CosignVerification{
			PublicKeySecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cosign-key"},
				Key:                  "cosign.pub",
			},
		},
	}

	err := m.verifyOperatorCatalogSignature(context.Background(), mt, op, "sha256:1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("expected error for an invalid PEM public key")
	}
}

// --- destinationDigest ---

func TestDestinationDigest(t *testing.T) {
	cases := []struct {
		name string
		dest string
		want string
	}{
		{"component destination (digest-derived tag)", "registry.io/ns/repo:sha256-abcd1234", "sha256:abcd1234"},
		{"digest reference", "registry.io/ns/repo@sha256:abcd1234", "sha256:abcd1234"},
		{"tag-only, no digest", "registry.io/ns/repo:v1.0", ""},
		{"bare repo, no tag or digest", "registry.io/ns/repo", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := destinationDigest(c.dest); got != c.want {
				t.Errorf("destinationDigest(%q) = %q, want %q", c.dest, got, c.want)
			}
		})
	}
}

// --- anyOwnerRequiresSignedImages ---

func TestAnyOwnerRequiresSignedImages(t *testing.T) {
	requireSignedByIS := map[string]bool{"strict-is": true, "lenient-is": false}

	if anyOwnerRequiresSignedImages(nil, requireSignedByIS) {
		t.Error("expected false for no owners")
	}
	if anyOwnerRequiresSignedImages([]string{"lenient-is"}, requireSignedByIS) {
		t.Error("expected false when the only owner does not require signed images")
	}
	if !anyOwnerRequiresSignedImages([]string{"lenient-is", "strict-is"}, requireSignedByIS) {
		t.Error("expected true when any owner requires signed images")
	}
	if anyOwnerRequiresSignedImages([]string{"unknown-is"}, requireSignedByIS) {
		t.Error("expected false for an owner not present in requireSignedByIS")
	}
}

// --- verifySignedImageLocked ---

// fakeSignaturePayload builds a "simple signing" payload attesting to digest.
func fakeSignaturePayload(digest string) []byte {
	p := map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": "example/repo"},
			"image":    map[string]any{"docker-manifest-digest": digest},
			"type":     "cosign container image signature",
		},
		"optional": map[string]any{},
	}
	b, _ := json.Marshal(p)
	return b
}

// fakeCosignSignatureServer serves a well-formed, validly-shaped cosign
// signature manifest at "<repo>:sha256-<hex>.sig" for imageDigest, signed
// with a freshly generated (non-attested, no embedded cert) key pair —
// enough to satisfy HasValidSignature's plain-key existence/shape check.
// Every other path 404s. Returns the registry host (no scheme).
func fakeCosignSignatureServer(t *testing.T, imageDigest string) string {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := signature.LoadECDSASigner(priv, crypto.SHA256)
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}
	payload := fakeSignaturePayload(imageDigest)
	sig, err := signer.SignMessage(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	configJSON := []byte(`{}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(configJSON))
	payloadDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))

	manifestJSON, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configJSON),
		},
		"layers": []map[string]any{{
			"mediaType":   "application/vnd.dev.cosign.simplesigning.v1+json",
			"digest":      payloadDigest,
			"size":        len(payload),
			"annotations": map[string]string{"dev.cosignproject.cosign/signature": sigB64},
		}},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestJSON))
	sigTag := "sha256-" + strings.TrimPrefix(imageDigest, "sha256:") + ".sig"

	blobs := map[string][]byte{configDigest: configJSON, payloadDigest: payload}

	mux := http.NewServeMux()
	mux.HandleFunc(registryPingPath, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == registryPingPath || path == "/v2":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(path, "/manifests/"+sigTag):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestJSON)
		case strings.Contains(path, "/blobs/"):
			for digest, content := range blobs {
				if strings.Contains(path, digest) {
					_, _ = w.Write(content)
					return
				}
			}
			http.NotFound(w, r)
		case strings.Contains(path, "/manifests/"):
			// Any other manifest tag (e.g. the image's own tag, as opposed to
			// its .sig tag) is reported as present, letting callers that need
			// both "the image exists" (CheckExist) and "its signature
			// verifies" (HasValidSignature) share a single fake server.
			// ManifestHead needs a media type and digest header to parse a
			// HEAD response at all — without them regclient errors, and
			// CheckExist's HTTP-then-HTTPS fallback masks that as a
			// "server gave HTTP response to HTTPS client" TLS error instead.
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", imageDigest)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// newTestManagerForSignatureCheck builds a MirrorManager with an
// initialized clientCache pre-warmed against host as an insecure (HTTP)
// registry, so subsequent internal m.clientCache.GetOrCreate(nil, "") calls
// (as verifySignedImageLocked makes) hit the same cached, insecure-capable
// client without needing to touch unexported ClientCache internals.
func newTestManagerForSignatureCheck(t *testing.T, host string) *MirrorManager {
	t.Helper()
	m := NewWithClients(nil, nil, "sig-test-target", "default", "test-image:latest", "", runtime.NewScheme())
	if _, err := m.clientCache.GetOrCreate([]string{host}, ""); err != nil {
		t.Fatalf("pre-warm client cache: %v", err)
	}
	return m
}

func TestVerifySignedImageLocked_ValidSignature_MarksVerified(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	host := fakeCosignSignatureServer(t, digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	dest := fmt.Sprintf("%s/example/repo:sha256-%s", host, strings.Repeat("a", 64))
	entry := &imagestate.ImageEntry{Source: "src", State: stateMirrored}

	m.mu.Lock()
	m.verifySignedImageLocked(context.Background(), dest, entry)
	m.mu.Unlock()

	if !entry.SignatureVerified {
		t.Error("expected SignatureVerified = true for a well-formed signature")
	}
	if entry.State != stateMirrored {
		t.Errorf("expected State to stay Mirrored, got %q", entry.State)
	}
	if !m.stateDirty {
		t.Error("expected stateDirty = true after a successful signature check")
	}
}

func TestVerifySignedImageLocked_MissingSignature_FailsEntry(t *testing.T) {
	// Registry with no signature manifest at all (404 for everything but the ping).
	mux := http.NewServeMux()
	mux.HandleFunc(registryPingPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registryPingPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	digestHex := strings.Repeat("b", 64)
	dest := fmt.Sprintf("%s/example/repo:sha256-%s", host, digestHex)
	m.mirrored[dest] = true
	entry := &imagestate.ImageEntry{Source: "src", State: stateMirrored}

	m.mu.Lock()
	m.verifySignedImageLocked(context.Background(), dest, entry)
	m.mu.Unlock()

	if entry.SignatureVerified {
		t.Error("expected SignatureVerified = false when no signature exists")
	}
	if entry.State != stateFailed {
		t.Errorf("expected State = Failed, got %q", entry.State)
	}
	if !strings.Contains(entry.LastError, "signature check failed") {
		t.Errorf("expected lastError to mention the signature check, got %q", entry.LastError)
	}
	if entry.RetryCount != 1 {
		t.Errorf("expected RetryCount = 1, got %d", entry.RetryCount)
	}
	if m.mirrored[dest] {
		t.Error("expected m.mirrored[dest] to be cleared so the next tick doesn't flip State back to Mirrored")
	}
}

func TestVerifySignedImageLocked_NoDigest_SkipsCheck(t *testing.T) {
	m := newTestManagerForSignatureCheck(t, "registry.example.com")
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	dest := "registry.example.com/example/repo:latest" // no digest
	entry := &imagestate.ImageEntry{Source: "src", State: stateMirrored}

	m.mu.Lock()
	m.verifySignedImageLocked(context.Background(), dest, entry)
	m.mu.Unlock()

	if entry.SignatureVerified {
		t.Error("expected SignatureVerified to stay false when no digest is available to check")
	}
	if entry.State != stateMirrored {
		t.Errorf("expected State to stay Mirrored (not failed) when the check is skipped, got %q", entry.State)
	}
	if m.stateDirty {
		t.Error("expected stateDirty to stay false when the check is skipped entirely")
	}
}
