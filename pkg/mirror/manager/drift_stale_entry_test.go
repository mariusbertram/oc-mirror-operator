package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// hookRegistry is a fake registry whose manifest requests for repo run hook
// (once per repo) before answering with digest — or 404 when digest is "".
// It lets a test change the manager's state exactly while the drift sweep
// waits on the network.
func hookRegistry(t *testing.T, answers map[string]string, hooks map[string]func()) string {
	t.Helper()
	var once sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registryPingPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		for repo, digest := range answers {
			if !strings.Contains(r.URL.Path, "/"+repo+"/manifests/") {
				continue
			}
			if hook := hooks[repo]; hook != nil {
				if _, done := once.LoadOrStore(repo, true); !done {
					hook()
				}
			}
			if digest == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// A worker callback changing the entry while CheckExist is in flight wins:
// the stale "missing" result must not reset it (#149).
func TestCheckDriftOne_EntryChangedDuringCheckExist(t *testing.T) {
	var m *MirrorManager
	var dest string
	host := hookRegistry(t, map[string]string{"repo": ""}, map[string]func(){
		"repo": func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.setImageStateLocked(dest, stateFailed, "worker failed meanwhile")
		},
	})
	m = newTestManagerForSignatureCheck(t, host)
	dest = host + "/repo:v1"
	m.imageState = imagestate.ImageState{dest: {Source: "src", State: stateMirrored}}
	m.owners = map[string][]string{dest: {"is"}}

	m.checkDriftOne(context.Background(), dest, nil)

	if got := m.imageState[dest]; got.State != stateFailed || got.LastError != "worker failed meanwhile" {
		t.Errorf("entry = %+v, want the worker's Failed state untouched", *got)
	}
}

// An entry replaced by a new object while the upstream digest of an
// additional image is resolved must be left alone, and no write may land
// on the detached old object (#149).
func TestCheckDriftOne_EntryReplacedDuringDigestLookup(t *testing.T) {
	const oldDigest, newDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	var m *MirrorManager
	var dest string
	replacement := &imagestate.ImageEntry{Source: "", State: stateMirrored, Origin: imagestate.OriginAdditional, SourceDigest: oldDigest}
	host := hookRegistry(t,
		map[string]string{"repo": oldDigest, "upstream": newDigest},
		map[string]func(){
			"upstream": func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				m.imageState[dest] = replacement // e.g. a resolve merge swapped the object
			},
		})
	m = newTestManagerForSignatureCheck(t, host)
	dest = host + "/repo:v1"
	source := host + "/upstream:latest"
	replacement.Source = source
	original := &imagestate.ImageEntry{Source: source, State: stateMirrored, Origin: imagestate.OriginAdditional, SourceDigest: oldDigest}
	m.imageState = imagestate.ImageState{dest: original}
	m.owners = map[string][]string{dest: {"is"}}

	m.checkDriftOne(context.Background(), dest, nil)

	// The result was computed for the same State/Source, so it is applied
	// to the current (replacement) entry — never to the detached original.
	if original.SourceDigest != oldDigest || original.State != stateMirrored {
		t.Errorf("detached original entry was modified: %+v", *original)
	}
	if replacement.State != statePending || replacement.SourceDigest != newDigest {
		t.Errorf("current entry = %+v, want drift applied to it", *replacement)
	}
}
