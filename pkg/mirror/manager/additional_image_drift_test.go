package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// fakeManifestServer serves a manifest whose Docker-Content-Digest header is
// read from *digest at request time, so a test can mutate *digest between
// calls to simulate an upstream tag moving to new content. Only responds to
// the ping and manifest HEAD/GET requests MirrorClient.GetDigest issues;
// everything else 404s. Returns the host (no scheme).
func fakeManifestServer(t *testing.T, digest *string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", *digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestAdditionalImageDriftedLocked_NoDrift(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	host := fakeManifestServer(t, &digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	entry := &imagestate.ImageEntry{
		Source:       fmt.Sprintf("%s/example/img:v1", host),
		State:        stateMirrored,
		Origin:       imagestate.OriginAdditional,
		SourceDigest: digest,
	}

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if drifted {
		t.Error("expected no drift when the upstream digest is unchanged")
	}
	if entry.SourceDigest != digest {
		t.Errorf("expected SourceDigest to remain %q, got %q", digest, entry.SourceDigest)
	}
}

func TestAdditionalImageDriftedLocked_Drifted(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	newDigest := "sha256:" + strings.Repeat("b", 64)
	digest := oldDigest
	host := fakeManifestServer(t, &digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	entry := &imagestate.ImageEntry{
		Source:       fmt.Sprintf("%s/example/img:v1", host),
		State:        stateMirrored,
		Origin:       imagestate.OriginAdditional,
		SourceDigest: oldDigest,
	}

	digest = newDigest // simulate the upstream tag moving to new content

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if !drifted {
		t.Error("expected drift to be detected when the upstream digest changed")
	}
	if entry.SourceDigest != newDigest {
		t.Errorf("expected SourceDigest to be updated to the new digest, got %q", entry.SourceDigest)
	}
	if !m.stateDirty {
		t.Error("expected stateDirty = true after recording a new SourceDigest")
	}
}

func TestAdditionalImageDriftedLocked_DigestPinnedSource_Skipped(t *testing.T) {
	m := newTestManagerForSignatureCheck(t, "registry.example.com")
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	digest := "sha256:" + strings.Repeat("c", 64)
	entry := &imagestate.ImageEntry{
		Source:       "registry.example.com/example/img@" + digest,
		State:        stateMirrored,
		Origin:       imagestate.OriginAdditional,
		SourceDigest: digest,
	}

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if drifted {
		t.Error("expected no drift check for a digest-pinned source, which cannot drift")
	}
}

func TestAdditionalImageDriftedLocked_NonAdditionalOrigin_Skipped(t *testing.T) {
	m := newTestManagerForSignatureCheck(t, "registry.example.com")
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	entry := &imagestate.ImageEntry{
		Source:       "registry.example.com/example/img:v1",
		State:        stateMirrored,
		Origin:       imagestate.OriginRelease,
		SourceDigest: "sha256:" + strings.Repeat("d", 64),
	}

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if drifted {
		t.Error("expected release-origin entries to never be drift-checked")
	}
}

func TestAdditionalImageDriftedLocked_NoBaseline_EstablishesOneWithoutFlaggingDrift(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	host := fakeManifestServer(t, &digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	entry := &imagestate.ImageEntry{
		Source: fmt.Sprintf("%s/example/img:v1", host),
		State:  stateMirrored,
		Origin: imagestate.OriginAdditional,
		// SourceDigest intentionally empty, e.g. state written before this
		// field existed.
	}

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if drifted {
		t.Error("expected no drift on the first check with no prior baseline")
	}
	if entry.SourceDigest != digest {
		t.Errorf("expected the resolved digest to become the new baseline, got %q", entry.SourceDigest)
	}
}

func TestAdditionalImageDriftedLocked_ResolutionFailure_NotDrifted(t *testing.T) {
	// Registry that 404s the manifest request entirely.
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

	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.mirrored = map[string]bool{}

	oldDigest := "sha256:" + strings.Repeat("f", 64)
	entry := &imagestate.ImageEntry{
		Source:       fmt.Sprintf("%s/example/img:v1", host),
		State:        stateMirrored,
		Origin:       imagestate.OriginAdditional,
		SourceDigest: oldDigest,
	}

	m.mu.Lock()
	drifted := m.additionalImageDriftedLocked(context.Background(), entry)
	m.mu.Unlock()

	if drifted {
		t.Error("expected a failed digest resolution to be treated as not drifted")
	}
	if entry.SourceDigest != oldDigest {
		t.Errorf("expected SourceDigest to be left unchanged on a resolution failure, got %q", entry.SourceDigest)
	}
}
