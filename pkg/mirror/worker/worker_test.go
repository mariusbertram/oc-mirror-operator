package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// fakeManager is an httptest stand-in for the manager's status API.
type fakeManager struct {
	mu        sync.Mutex
	gone      map[string]bool // dests answered with 410 on /should-mirror
	reports   []StatusRequest
	failNext  int      // number of /status requests to answer with 500
	queries   []string // raw queries of /should-mirror requests
	denyPrune bool     // answer prune confirmations with 410
	srv       *httptest.Server
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
			fm.queries = append(fm.queries, r.URL.RawQuery)
			if fm.gone[r.URL.Query().Get("dest")] || (fm.denyPrune && r.URL.Query().Get("prune") != "") {
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

const (
	callStream   = "stream"
	callBuffered = "buffered"
)

// bufferedFakeClient additionally implements BufferedCopier and records
// which copy strategy each attempt used.
type bufferedFakeClient struct {
	fakeClient
	calls []string
}

func (c *bufferedFakeClient) CopyImage(ctx context.Context, src, dest string) (string, error) {
	c.calls = append(c.calls, callStream)
	return c.fakeClient.CopyImage(ctx, src, dest)
}

func (c *bufferedFakeClient) CopyImageBuffered(ctx context.Context, src, dest string) (string, error) {
	c.calls = append(c.calls, callBuffered)
	return c.fakeClient.CopyImage(ctx, src, dest)
}

func TestMirrorOne_FallsBackToBufferedCopy(t *testing.T) {
	tests := []struct {
		name      string
		errs      []error
		wantCalls []string
		wantOK    bool
	}{
		{name: "stream succeeds", errs: nil, wantCalls: []string{callStream}, wantOK: true},
		{name: "buffered after failed stream", errs: []error{errors.New("BLOB_UPLOAD_UNKNOWN")}, wantCalls: []string{callStream, callBuffered}, wantOK: true},
		{name: "both fail", errs: []error{errors.New("a"), errors.New("b")}, wantCalls: []string{callStream, callBuffered}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm := newFakeManager(t)
			w := &Worker{Status: fm.status()}
			var copied []string
			c := &bufferedFakeClient{fakeClient: fakeClient{
				copyErrs:  map[string][]error{"reg/0": tt.errs},
				digestErr: map[string]error{},
				copied:    &copied,
			}}
			if got := w.MirrorOne(context.Background(), c, "src/0", "reg/0"); got != tt.wantOK {
				t.Fatalf("MirrorOne = %v, want %v", got, tt.wantOK)
			}
			if len(c.calls) != len(tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", c.calls, tt.wantCalls)
			}
			for i := range tt.wantCalls {
				if c.calls[i] != tt.wantCalls[i] {
					t.Fatalf("calls = %v, want %v", c.calls, tt.wantCalls)
				}
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

// skipMissingFakeClient records which copy path the worker chose per dest.
type skipMissingFakeClient struct {
	bufferedFakeClient
	skipCalls map[string][]bool // dest → buffered flag of each filtered call
	filters   map[string]mirrorclient.CopyFilter
	skipped   []string
}

func (c *skipMissingFakeClient) CopyImageFiltered(ctx context.Context, src, dest string, f mirrorclient.CopyFilter, buffered bool) (string, []string, error) {
	c.skipCalls[dest] = append(c.skipCalls[dest], buffered)
	if c.filters != nil {
		c.filters[dest] = f
	}
	eff, err := c.fakeClient.CopyImage(ctx, src, dest)
	return eff, c.skipped, err
}

func TestRunBatch_SkipMissingArchitecturesPerItem(t *testing.T) {
	fm := newFakeManager(t)
	var copied []string
	c := &skipMissingFakeClient{
		bufferedFakeClient: bufferedFakeClient{fakeClient: fakeClient{
			copyErrs: map[string][]error{}, digestErr: map[string]error{}, copied: &copied,
		}},
		skipCalls: map[string][]bool{},
		filters:   map[string]mirrorclient.CopyFilter{},
		skipped:   []string{"linux/s390x sha256:abc"},
	}
	w := &Worker{
		NewClient: func(string) Client { return c },
		Status:    fm.status(),
		// Reverse the order to check the flag follows its dest, not its index.
		Plan: func(_ context.Context, _ Client, s, d []string) ([]string, []string) {
			return []string{s[1], s[0]}, []string{d[1], d[0]}
		},
	}
	w.RunBatch(context.Background(), []BatchItem{
		{Source: "src/0", Dest: "reg/0", SkipMissingArchitectures: true, Architectures: []string{"amd64"}},
		{Source: "src/1", Dest: "reg/1"},
	})
	if f := c.filters["reg/0"]; !f.SkipMissing || len(f.Architectures) != 1 || f.Architectures[0] != "amd64" {
		t.Errorf("reg/0 filter = %+v", f)
	}
	if got := c.skipCalls["reg/0"]; len(got) != 1 || got[0] {
		t.Errorf("reg/0 skip-missing calls = %v, want one streamed call", got)
	}
	if f := c.filters["reg/1"]; !f.ForceRecursive || f.SkipMissing || len(f.Architectures) != 0 {
		t.Errorf("reg/1 must be a plain but recursive copy, got %+v", f)
	}
	if len(copied) != 2 {
		t.Errorf("copied = %v, want both", copied)
	}
	reports := fm.reportsByDest()
	if reports["reg/0"].Error != "" || reports["reg/1"].Error != "" {
		t.Errorf("reports = %v, want both successful", reports)
	}
}

func TestMirrorOne_SkipMissingUsesBufferedOnLastAttempt(t *testing.T) {
	fm := newFakeManager(t)
	var copied []string
	c := &skipMissingFakeClient{
		bufferedFakeClient: bufferedFakeClient{fakeClient: fakeClient{
			copyErrs:  map[string][]error{"reg/0": {errors.New("BLOB_UPLOAD_UNKNOWN")}},
			digestErr: map[string]error{},
			copied:    &copied,
		}},
		skipCalls: map[string][]bool{},
	}
	w := &Worker{Status: fm.status()}
	if !w.mirrorOne(context.Background(), c, BatchItem{Source: "src/0", Dest: "reg/0", SkipMissingArchitectures: true}) {
		t.Fatal("mirrorOne failed")
	}
	if got := c.skipCalls["reg/0"]; len(got) != 2 || got[0] || !got[1] {
		t.Errorf("skip-missing calls = %v, want [streamed, buffered]", got)
	}
}

func TestBatchItem_FilterJSON(t *testing.T) {
	items, err := ParseBatch(`[{"source":"s","dest":"d","skipMissingArchitectures":true,"architectures":["amd64","arm64"]},{"source":"s2","dest":"d2"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !items[0].SkipMissingArchitectures || len(items[0].Architectures) != 2 || items[1].SkipMissingArchitectures || items[1].filter().SkipMissing || len(items[1].filter().Architectures) != 0 {
		t.Errorf("items = %+v", items)
	}
}

// prunerFakeClient adds PruneArchitectures to skipMissingFakeClient.
type prunerFakeClient struct {
	skipMissingFakeClient
	pruneErr   error
	pruneCalls map[string][]string // dest → retain
}

func (c *prunerFakeClient) PruneArchitectures(_ context.Context, dest string, _, retain []string) ([]string, error) {
	c.pruneCalls[dest] = retain
	if c.pruneErr != nil {
		return nil, c.pruneErr
	}
	return []string{"linux/s390x sha256:abc"}, nil
}

func newPrunerFake() (*prunerFakeClient, *[]string) {
	var copied []string
	return &prunerFakeClient{
		skipMissingFakeClient: skipMissingFakeClient{
			bufferedFakeClient: bufferedFakeClient{fakeClient: fakeClient{
				copyErrs: map[string][]error{}, digestErr: map[string]error{}, copied: &copied,
			}},
			skipCalls: map[string][]bool{},
			filters:   map[string]mirrorclient.CopyFilter{},
		},
		pruneCalls: map[string][]string{},
	}, &copied
}

func TestRunBatch_ArchitectureReconcile(t *testing.T) {
	fm := newFakeManager(t)
	c, copied := newPrunerFake()
	w := &Worker{NewClient: func(string) Client { return c }, Status: fm.status()}
	w.RunBatch(context.Background(), []BatchItem{
		{Source: "src/backfill", Dest: "reg/backfill", Architectures: []string{"amd64", "arm64"}, Backfill: true},
		{Source: "src/prune", Dest: "reg/prune", Architectures: []string{"amd64"}, PruneRetain: []string{"amd64", "arm64"}},
		{Source: "src/plain", Dest: "reg/plain"},
	})

	// Backfill copies with ForceRecursive, prune-only does not copy.
	if f := c.filters["reg/backfill"]; !f.ForceRecursive || len(f.Architectures) != 2 {
		t.Errorf("backfill filter = %+v", f)
	}
	if _, ok := c.filters["reg/prune"]; ok {
		t.Error("prune-only reconcile must not copy")
	}
	if len(*copied) != 2 {
		t.Errorf("copied = %v, want backfill and plain", *copied)
	}
	if f := c.filters["reg/plain"]; !f.ForceRecursive {
		t.Errorf("plain item must copy recursively: %+v", f)
	}
	if got := c.pruneCalls["reg/prune"]; len(got) != 2 {
		t.Errorf("prune retain = %v", got)
	}
	if _, ok := c.pruneCalls["reg/backfill"]; ok {
		t.Error("backfill without PruneRetain must not prune")
	}

	// Reconcile items ask /should-mirror with reconcile=1, and the prune is
	// confirmed once more right before deleting.
	reconcileQueries, pruneConfirms := 0, 0
	for _, q := range fm.queries {
		if strings.Contains(q, "reconcile=1") {
			reconcileQueries++
		}
		if strings.Contains(q, "prune=amd64%2Carm64") {
			pruneConfirms++
		}
	}
	if reconcileQueries != 3 || pruneConfirms != 1 {
		t.Errorf("reconcile queries = %d (want 3), prune confirmations = %d (want 1): %v", reconcileQueries, pruneConfirms, fm.queries)
	}

	reports := fm.reportsByDest()
	for _, d := range []string{"reg/backfill", "reg/prune"} {
		if r := reports[d]; !r.Reconcile || r.Error != "" || r.Digest == "" {
			t.Errorf("%s report = %+v", d, r)
		}
	}
	if r := reports["reg/prune"]; len(r.Architectures) != 1 || r.Architectures[0] != "amd64" {
		t.Errorf("prune report architectures = %v", r.Architectures)
	}
	if r := reports["reg/plain"]; r.Reconcile || r.Architectures != nil {
		t.Errorf("plain report = %+v", r)
	}
}

func TestMirrorOne_ReconcileFailures(t *testing.T) {
	t.Run("prune error", func(t *testing.T) {
		fm := newFakeManager(t)
		c, _ := newPrunerFake()
		c.pruneErr = errors.New("DELETE not allowed")
		w := &Worker{Status: fm.status()}
		if w.mirrorOne(context.Background(), c, BatchItem{Source: "s", Dest: "reg/x", PruneRetain: []string{"amd64"}}) {
			t.Fatal("expected failure")
		}
		if r := fm.reportsByDest()["reg/x"]; !r.Reconcile || !strings.Contains(r.Error, "DELETE not allowed") {
			t.Errorf("report = %+v", r)
		}
	})
	t.Run("client cannot prune", func(t *testing.T) {
		fm := newFakeManager(t)
		var copied []string
		c := &fakeClient{copyErrs: map[string][]error{}, digestErr: map[string]error{}, copied: &copied}
		w := &Worker{Status: fm.status()}
		if w.mirrorOne(context.Background(), c, BatchItem{Source: "s", Dest: "reg/x", PruneRetain: []string{"amd64"}}) {
			t.Fatal("expected failure")
		}
		if r := fm.reportsByDest()["reg/x"]; !r.Reconcile || r.Error == "" {
			t.Errorf("report = %+v", r)
		}
	})
	t.Run("backfill copy error", func(t *testing.T) {
		fm := newFakeManager(t)
		c, _ := newPrunerFake()
		c.copyErrs["reg/x"] = []error{errors.New("a"), errors.New("b")}
		w := &Worker{Status: fm.status()}
		if w.mirrorOne(context.Background(), c, BatchItem{Source: "s", Dest: "reg/x", Backfill: true, PruneRetain: []string{"amd64"}}) {
			t.Fatal("expected failure")
		}
		if _, pruned := c.pruneCalls["reg/x"]; pruned {
			t.Error("must not prune after a failed backfill")
		}
		if r := fm.reportsByDest()["reg/x"]; !r.Reconcile || r.Error == "" {
			t.Errorf("report = %+v", r)
		}
	})
}

func TestStatusClient_ShouldReconcileUnconfigured(t *testing.T) {
	var s *StatusClient
	if !s.ShouldReconcile(context.Background(), "d") {
		t.Error("unconfigured client must answer true")
	}
}

// Review #299 P1: a prune the manager no longer confirms (policy removed,
// another ImageSet needs the architecture) must not delete anything.
func TestMirrorOne_PruneNotConfirmed(t *testing.T) {
	fm := newFakeManager(t)
	fm.denyPrune = true
	c, _ := newPrunerFake()
	w := &Worker{Status: fm.status()}
	if w.mirrorOne(context.Background(), c, BatchItem{Source: "s", Dest: "reg/x", Architectures: []string{"amd64"}, PruneRetain: []string{"amd64"}}) {
		t.Fatal("expected the reconcile to fail")
	}
	if _, pruned := c.pruneCalls["reg/x"]; pruned {
		t.Error("must not prune without confirmation")
	}
	if r := fm.reportsByDest()["reg/x"]; !r.Reconcile || !strings.Contains(r.Error, "did not confirm") {
		t.Errorf("report = %+v", r)
	}
}

func TestStatusClient_ConfirmPruneFailsClosed(t *testing.T) {
	var s *StatusClient
	if s.ConfirmPrune(context.Background(), "d", []string{"amd64"}) {
		t.Error("unconfigured client must not confirm")
	}
	unreachable := &StatusClient{ManagerURL: "http://127.0.0.1:1", Token: "t"}
	if unreachable.ConfirmPrune(context.Background(), "d", []string{"amd64"}) {
		t.Error("unreachable manager must not confirm")
	}
	bad := &StatusClient{ManagerURL: "://bad", Token: "t"}
	if bad.ConfirmPrune(context.Background(), "d", []string{"amd64"}) {
		t.Error("bad URL must not confirm")
	}
}
