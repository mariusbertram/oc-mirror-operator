package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeManager is an httptest stand-in for the manager's status API.
type fakeManager struct {
	mu       sync.Mutex
	gone     map[string]bool // dests answered with 410 on /should-mirror
	reports  []StatusRequest
	failNext int // number of /status requests to answer with 500
	srv      *httptest.Server
}

func newFakeManager(t *testing.T) *fakeManager {
	t.Helper()
	fm := &fakeManager{gone: map[string]bool{}}
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fm.mu.Lock()
		defer fm.mu.Unlock()
		switch r.URL.Path {
		case "/should-mirror":
			if fm.gone[r.URL.Query().Get("dest")] {
				w.WriteHeader(http.StatusGone)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/status":
			if fm.failNext > 0 {
				fm.failNext--
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var req StatusRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			fm.reports = append(fm.reports, req)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

func (fm *fakeManager) status() *StatusClient {
	return &StatusClient{ManagerURL: fm.srv.URL, PodName: "pod", Token: "tok"}
}

func (fm *fakeManager) reportsByDest() map[string]StatusRequest {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	out := map[string]StatusRequest{}
	for _, r := range fm.reports {
		out[r.Destination] = r
	}
	return out
}

// fakeClient is a registry client whose copy and verify results are
// scripted per destination.
type fakeClient struct {
	id        int
	copyErrs  map[string][]error // consumed one per attempt
	digestErr map[string]error
	copied    *[]string
}

func (c *fakeClient) CopyImage(_ context.Context, _, dest string) (string, error) {
	if errs := c.copyErrs[dest]; len(errs) > 0 {
		c.copyErrs[dest] = errs[1:]
		if errs[0] != nil {
			return "", errs[0]
		}
	}
	*c.copied = append(*c.copied, dest)
	return dest, nil
}

func (c *fakeClient) GetDigest(_ context.Context, image string) (string, error) {
	if err := c.digestErr[image]; err != nil {
		return "", err
	}
	return "sha256:" + image, nil
}

type harness struct {
	w         *Worker
	fm        *fakeManager
	clientsAt []string // firstDest of every NewClient call
	copied    []string
	copyErrs  map[string][]error
	digestErr map[string]error
}

func newHarness(t *testing.T) *harness {
	h := &harness{fm: newFakeManager(t), copyErrs: map[string][]error{}, digestErr: map[string]error{}}
	h.w = &Worker{
		NewClient: func(firstDest string) Client {
			h.clientsAt = append(h.clientsAt, firstDest)
			return &fakeClient{id: len(h.clientsAt), copyErrs: h.copyErrs, digestErr: h.digestErr, copied: &h.copied}
		},
		Status: h.fm.status(),
	}
	return h
}

func items(n int) []BatchItem {
	out := make([]BatchItem, n)
	for i := range out {
		out[i] = BatchItem{Source: "src/" + strconv.Itoa(i), Dest: "reg/" + strconv.Itoa(i)}
	}
	return out
}

func TestRunBatch(t *testing.T) {
	tests := []struct {
		name        string
		n           int
		setup       func(h *harness)
		wantFailed  bool
		wantCopied  int
		wantClients []string
		check       func(t *testing.T, h *harness)
	}{
		{
			name:        "all succeed and report digests",
			n:           3,
			wantCopied:  3,
			wantClients: []string{"reg/0"},
			check: func(t *testing.T, h *harness) {
				r := h.fm.reportsByDest()
				if len(r) != 3 || r["reg/1"].Digest != "sha256:reg/1" || r["reg/1"].Error != "" || r["reg/1"].PodName != "pod" {
					t.Fatalf("reports = %+v", r)
				}
			},
		},
		{
			name:        "skips images the manager answers 410 for",
			n:           3,
			setup:       func(h *harness) { h.fm.gone["reg/1"] = true },
			wantCopied:  2,
			wantClients: []string{"reg/0"},
			check: func(t *testing.T, h *harness) {
				if _, reported := h.fm.reportsByDest()["reg/1"]; reported {
					t.Fatal("skipped image was reported")
				}
			},
		},
		{
			name:        "refreshes the client every ClientRefreshInterval images",
			n:           2*ClientRefreshInterval + 1,
			wantCopied:  2*ClientRefreshInterval + 1,
			wantClients: []string{"reg/0", "reg/20", "reg/40"},
		},
		{
			name: "retries a failed copy once",
			n:    1,
			setup: func(h *harness) {
				h.copyErrs["reg/0"] = []error{errors.New("transient")}
			},
			wantCopied:  1,
			wantClients: []string{"reg/0"},
		},
		{
			name: "reports a copy that fails every attempt",
			n:    2,
			setup: func(h *harness) {
				h.copyErrs["reg/0"] = []error{errors.New("boom"), errors.New("boom")}
			},
			wantFailed:  true,
			wantCopied:  1,
			wantClients: []string{"reg/0"},
			check: func(t *testing.T, h *harness) {
				r := h.fm.reportsByDest()
				if r["reg/0"].Error != "boom" || r["reg/1"].Error != "" {
					t.Fatalf("reports = %+v", r)
				}
			},
		},
		{
			name:        "reports a failed digest verification",
			n:           1,
			setup:       func(h *harness) { h.digestErr["reg/0"] = errors.New("no manifest") },
			wantFailed:  true,
			wantCopied:  1,
			wantClients: []string{"reg/0"},
			check: func(t *testing.T, h *harness) {
				if got := h.fm.reportsByDest()["reg/0"].Error; got != "no manifest" {
					t.Fatalf("error = %q", got)
				}
			},
		},
		{
			name: "empty batch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			if got := h.w.RunBatch(context.Background(), items(tt.n)); got != tt.wantFailed {
				t.Fatalf("anyFailed = %v, want %v", got, tt.wantFailed)
			}
			if len(h.copied) != tt.wantCopied {
				t.Fatalf("copied %d images, want %d", len(h.copied), tt.wantCopied)
			}
			if len(h.clientsAt) != len(tt.wantClients) {
				t.Fatalf("clients built at %v, want %v", h.clientsAt, tt.wantClients)
			}
			for i := range tt.wantClients {
				if h.clientsAt[i] != tt.wantClients[i] {
					t.Fatalf("clients built at %v, want %v", h.clientsAt, tt.wantClients)
				}
			}
			if tt.check != nil {
				tt.check(t, h)
			}
		})
	}
}

func TestRunBatch_UsesPlannedOrder(t *testing.T) {
	h := newHarness(t)
	h.w.Plan = func(_ context.Context, _ Client, s, d []string) ([]string, []string) {
		return []string{s[1], s[0]}, []string{d[1], d[0]}
	}
	h.w.RunBatch(context.Background(), items(2))
	if len(h.copied) != 2 || h.copied[0] != "reg/1" {
		t.Fatalf("copy order = %v", h.copied)
	}
}

func TestPlanWithMirrorClient_KeepsOrderForOtherClients(t *testing.T) {
	s, d := planWithMirrorClient(context.Background(), &fakeClient{}, []string{"a", "b"}, []string{"x", "y"})
	if s[0] != "a" || d[1] != "y" {
		t.Fatalf("got %v %v", s, d)
	}
}

func TestStatusClient_ReportRetries(t *testing.T) {
	fm := newFakeManager(t)
	fm.failNext = StatusAttempts - 1
	fm.status().Report(context.Background(), "d", "sha256:x", "")
	if r := fm.reportsByDest()["d"]; r.Digest != "sha256:x" {
		t.Fatalf("report after retries = %+v", r)
	}

	fm.failNext = StatusAttempts
	fm.status().Report(context.Background(), "gave-up", "", "")
	if _, ok := fm.reportsByDest()["gave-up"]; ok {
		t.Fatal("report delivered although every attempt failed")
	}
}

func TestStatusClient_Unconfigured(t *testing.T) {
	var nilClient *StatusClient
	nilClient.Report(context.Background(), "d", "", "") // must not panic
	if !nilClient.ShouldMirror(context.Background(), "d") {
		t.Fatal("nil client must fail open")
	}
	s := &StatusClient{}
	s.Report(context.Background(), "d", "", "")
	if !s.ShouldMirror(context.Background(), "d") {
		t.Fatal("unconfigured client must fail open")
	}
}

func TestStatusClient_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused from now on
	s := &StatusClient{ManagerURL: url, PodName: "p", Token: "tok"}
	if !s.ShouldMirror(context.Background(), "d") {
		t.Fatal("unreachable manager must fail open")
	}
	s.Report(context.Background(), "d", "", "") // logs and gives up

	bad := &StatusClient{ManagerURL: "http://bad host", PodName: "p"}
	if !bad.ShouldMirror(context.Background(), "d") {
		t.Fatal("malformed URL must fail open")
	}
	bad.Report(context.Background(), "d", "", "")
}

func TestStatusClientFromEnv(t *testing.T) {
	t.Setenv("MANAGER_URL", "http://m")
	t.Setenv("POD_NAME", "p")
	t.Setenv("WORKER_TOKEN", "t")
	s := StatusClientFromEnv()
	if s.ManagerURL != "http://m" || s.PodName != "p" || s.Token != "t" || s.RetryDelay != StatusRetryDelay {
		t.Fatalf("got %+v", s)
	}
}

func TestNewAndNewMirrorClient(t *testing.T) {
	w := New(true)
	if w.NewClient("reg.io/ns/img:v1") == nil || w.Plan == nil || w.RetryDelay != CopyRetryDelay {
		t.Fatalf("New() = %+v", w)
	}
	if NewMirrorClient(false, "") == nil {
		t.Fatal("NewMirrorClient returned nil")
	}
}

func TestImageBudget(t *testing.T) {
	// Two 20 min copy attempts, 15 s between them, 2 min verification.
	if want := 42*60 + 15; int(ImageBudget.Seconds()) != want {
		t.Fatalf("ImageBudget = %s", ImageBudget)
	}
}
