package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"k8s.io/apimachinery/pkg/runtime"
)

// fakeExistenceServer serves a fake registry whose manifest endpoint always
// reports either present (200) or absent (404), for exercising the
// CheckExist "found"/"not found" outcomes deterministically. Returns the
// host (no scheme).
func fakeExistenceServer(t *testing.T, exists bool) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(registryPingPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registryPingPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			if !exists {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("9", 64))
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// fakeBadRequestTLSServer serves a fake HTTPS registry whose manifest
// endpoint always returns 400 Bad Request, for exercising
// checkExistNoLock's "discard the cached client and retry once" path. A TLS
// server is used (rather than plain HTTP) so that MirrorClient's automatic
// primary(HTTP)/fallback(HTTPS skip-verify) retry inside CheckExist itself
// still lands on the controlled 400 response via the fallback leg, instead
// of masking it with a transport-level error. Returns the host (no scheme).
func fakeBadRequestTLSServer(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(registryPingPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registryPingPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

// ─── checkExistNoLock ──────────────────────────────────────────────────

func TestCheckExistNoLock_Exists(t *testing.T) {
	host := fakeExistenceServer(t, true)
	m := newTestManagerForSignatureCheck(t, host)

	exists, err := m.checkExistNoLock(context.Background(), fmt.Sprintf("%s/repo:v1", host))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestCheckExistNoLock_NotFound(t *testing.T) {
	host := fakeExistenceServer(t, false)
	m := newTestManagerForSignatureCheck(t, host)

	exists, err := m.checkExistNoLock(context.Background(), fmt.Sprintf("%s/repo:v1", host))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected exists=false")
	}
}

func TestCheckExistNoLock_NonRetryableError_ReturnsImmediately(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	before, _ := m.clientCache.GetOrCreate(nil, "")

	// A malformed reference fails ref.New before any HTTP round-trip, so the
	// resulting error is neither errs.ErrHTTPStatus nor "400" — the retry
	// branch must not fire.
	exists, err := m.checkExistNoLock(context.Background(), ":::invalid")
	if err == nil {
		t.Fatal("expected an error for a malformed reference")
	}
	if exists {
		t.Error("expected exists=false on error")
	}

	after, _ := m.clientCache.GetOrCreate(nil, "")
	if before != after {
		t.Error("expected the cached client to be left untouched for a non-400 error")
	}
}

func TestCheckExistNoLock_400_DiscardsCachedClientAndRetries(t *testing.T) {
	host := fakeBadRequestTLSServer(t)
	m := newTestManagerForSignatureCheck(t, host)
	before, _ := m.clientCache.GetOrCreate(nil, "")

	exists, err := m.checkExistNoLock(context.Background(), fmt.Sprintf("%s/repo:v1", host))
	if exists {
		t.Error("expected exists=false")
	}
	if err == nil {
		t.Fatal("expected an error from the retried request")
	}
	if strings.Contains(err.Error(), "[http 400]") {
		t.Errorf("expected the returned error to come from the fresh (unconfigured) client, not the original 400: %v", err)
	}

	after, _ := m.clientCache.GetOrCreate(nil, "")
	if before == after {
		t.Error("expected checkExistNoLock to discard the cached client and install a fresh one after a 400")
	}
}

// ─── checkDriftOne ─────────────────────────────────────────────────────

func newDriftTestManager() *MirrorManager {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}
	return m
}

func TestCheckDriftOne_NoEntry_NoOp(t *testing.T) {
	m := newDriftTestManager()
	m.checkDriftOne(context.Background(), "reg.io/missing:v1", nil)
	if len(m.imageState) != 0 || len(m.mirrored) != 0 {
		t.Error("expected no state to be created for an unknown destination")
	}
}

func TestCheckDriftOne_NoOwners_NoOp(t *testing.T) {
	m := newDriftTestManager()
	dest := "reg.io/img:v1"
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: stateMirrored}
	before := *m.imageState[dest]

	m.checkDriftOne(context.Background(), dest, nil)

	if *m.imageState[dest] != before {
		t.Errorf("expected entry untouched when it has no current owner, got %+v", m.imageState[dest])
	}
}

func TestCheckDriftOne_NonMirroredNonPermanentlyFailed_NoOp(t *testing.T) {
	m := newDriftTestManager()
	dest := "reg.io/img:v1"
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: statePending}
	m.owners[dest] = []string{"is-a"}
	before := *m.imageState[dest]

	m.checkDriftOne(context.Background(), dest, nil)

	if *m.imageState[dest] != before {
		t.Errorf("expected a Pending entry to be left untouched, got %+v", m.imageState[dest])
	}
}

func TestCheckDriftOne_Mirrored_Exists_TrustedNoChange(t *testing.T) {
	host := fakeExistenceServer(t, true)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}
	dest := fmt.Sprintf("%s/repo:v1", host)
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: stateMirrored}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	if !m.mirrored[dest] {
		t.Error("expected m.mirrored[dest] = true when the image still exists")
	}
	if m.imageState[dest].State != stateMirrored {
		t.Errorf("expected State to stay Mirrored, got %q", m.imageState[dest].State)
	}
}

func TestCheckDriftOne_Mirrored_NotFound_ResetsToPending(t *testing.T) {
	host := fakeExistenceServer(t, false)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}
	dest := fmt.Sprintf("%s/repo:v1", host)
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: stateMirrored, RetryCount: 3, LastError: "old"}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	entry := m.imageState[dest]
	if entry.State != statePending {
		t.Errorf("expected State reset to Pending, got %q", entry.State)
	}
	if entry.RetryCount != 0 || entry.LastError != "" {
		t.Errorf("expected RetryCount/LastError cleared, got %d/%q", entry.RetryCount, entry.LastError)
	}
	if !m.stateDirty {
		t.Error("expected stateDirty = true")
	}
}

func TestCheckDriftOne_Mirrored_CheckErr_AssumesPresent(t *testing.T) {
	m := newDriftTestManager()
	dest := "no-such-registry.invalid/repo:v1"
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: stateMirrored}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	if !m.mirrored[dest] {
		t.Error("expected a CheckExist error to be treated as 'assume present'")
	}
	if m.imageState[dest].State != stateMirrored {
		t.Errorf("expected State to stay Mirrored, got %q", m.imageState[dest].State)
	}
}

func TestCheckDriftOne_AdditionalOriginDrifted_ResetsToPending(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	host := fakeManifestServer(t, &digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}

	dest := fmt.Sprintf("%s/mirror/img:v1", host)
	m.imageState[dest] = &imagestate.ImageEntry{
		Source:       fmt.Sprintf("%s/upstream/img:v1", host),
		State:        stateMirrored,
		Origin:       imagestate.OriginAdditional,
		SourceDigest: "sha256:" + strings.Repeat("2", 64), // stale baseline, differs from digest above
	}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	entry := m.imageState[dest]
	if entry.State != statePending {
		t.Errorf("expected State reset to Pending after upstream drift, got %q", entry.State)
	}
	if entry.SourceDigest != digest {
		t.Errorf("expected SourceDigest updated to the new baseline %q, got %q", digest, entry.SourceDigest)
	}
}

func TestCheckDriftOne_RequiresSignature_VerifiesAndMarks(t *testing.T) {
	digestHex := strings.Repeat("3", 64)
	digest := "sha256:" + digestHex
	host := fakeCosignSignatureServer(t, digest)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}

	dest := fmt.Sprintf("%s/example/repo:sha256-%s", host, digestHex)
	m.imageState[dest] = &imagestate.ImageEntry{Source: "src", State: stateMirrored}
	m.owners[dest] = []string{"strict-is"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{"strict-is": true})

	entry := m.imageState[dest]
	if !entry.SignatureVerified {
		t.Error("expected SignatureVerified = true after the drift check verifies a valid signature")
	}
	if !m.mirrored[dest] {
		t.Error("expected m.mirrored[dest] = true")
	}
}

func TestCheckDriftOne_PermanentlyFailedRecovery_ExistsMarksMirrored(t *testing.T) {
	host := fakeExistenceServer(t, true)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}
	dest := fmt.Sprintf("%s/repo:v1", host)
	m.imageState[dest] = &imagestate.ImageEntry{
		Source: "src", State: stateFailed, PermanentlyFailed: true, RetryCount: 10, LastError: "boom",
	}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	entry := m.imageState[dest]
	if entry.State != stateMirrored {
		t.Errorf("expected State = Mirrored, got %q", entry.State)
	}
	if entry.LastError != "" {
		t.Errorf("expected LastError cleared, got %q", entry.LastError)
	}
	if !entry.PermanentlyFailed {
		t.Error("expected the sticky PermanentlyFailed marker to remain true")
	}
	if !m.mirrored[dest] {
		t.Error("expected m.mirrored[dest] = true")
	}
}

func TestCheckDriftOne_PermanentlyFailedRecovery_NotFoundResetsForRetry(t *testing.T) {
	host := fakeExistenceServer(t, false)
	m := newTestManagerForSignatureCheck(t, host)
	m.imageState = imagestate.ImageState{}
	m.owners = map[string][]string{}
	m.mirrored = map[string]bool{}
	dest := fmt.Sprintf("%s/repo:v1", host)
	m.imageState[dest] = &imagestate.ImageEntry{
		Source: "src", State: stateFailed, PermanentlyFailed: true, RetryCount: 10,
	}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	entry := m.imageState[dest]
	if entry.State != statePending {
		t.Errorf("expected State reset to Pending for a fresh retry window, got %q", entry.State)
	}
	if entry.RetryCount != 0 {
		t.Errorf("expected RetryCount reset to 0, got %d", entry.RetryCount)
	}
	if !entry.PermanentlyFailed {
		t.Error("expected the sticky PermanentlyFailed marker to remain true")
	}
}

func TestCheckDriftOne_PermanentlyFailedRecovery_CheckErrKeepsFailed(t *testing.T) {
	m := newDriftTestManager()
	dest := "no-such-registry.invalid/repo:v1"
	m.imageState[dest] = &imagestate.ImageEntry{
		Source: "src", State: stateFailed, PermanentlyFailed: true, RetryCount: 10,
	}
	m.owners[dest] = []string{"is-a"}

	m.checkDriftOne(context.Background(), dest, map[string]bool{})

	entry := m.imageState[dest]
	if entry.State != stateFailed {
		t.Errorf("expected State to stay Failed when the recovery check itself errors, got %q", entry.State)
	}
	if entry.RetryCount != 10 {
		t.Errorf("expected RetryCount untouched, got %d", entry.RetryCount)
	}
}
