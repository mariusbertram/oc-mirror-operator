package manager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/worker"
)

func TestWorkerPodStuckPending(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		phase corev1.PodPhase
		age   time.Duration
		want  bool
	}{
		{"fresh pending", corev1.PodPending, time.Minute, false},
		{"old pending", corev1.PodPending, workerPendingTimeout + time.Minute, true},
		{"old running", corev1.PodRunning, 10 * workerPendingTimeout, false},
		{"old failed", corev1.PodFailed, 10 * workerPendingTimeout, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-tt.age))},
				Status:     corev1.PodStatus{Phase: tt.phase},
			}
			if got := workerPodStuckPending(pod, now); got != tt.want {
				t.Errorf("workerPodStuckPending() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("no creation timestamp", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
		if workerPodStuckPending(pod, now) {
			t.Error("expected a pod without creation timestamp not to be considered stuck")
		}
	})
}

func TestWorkerActiveDeadlineSeconds(t *testing.T) {
	perImage := int64(workerImageBudget / time.Second)
	tests := []struct {
		batchLen int
		want     int64
	}{
		{0, perImage},
		{1, perImage},
		{50, 50 * perImage},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("batch=%d", tt.batchLen), func(t *testing.T) {
			if got := workerActiveDeadlineSeconds(tt.batchLen); got != tt.want {
				t.Errorf("workerActiveDeadlineSeconds(%d) = %d, want %d", tt.batchLen, got, tt.want)
			}
		})
	}
}

func TestHandleHealthz(t *testing.T) {
	tests := []struct {
		name      string
		heartbeat time.Time
		want      int
	}{
		{"no heartbeat yet", time.Time{}, http.StatusOK},
		{"recent heartbeat", time.Now(), http.StatusOK},
		{"stale heartbeat", time.Now().Add(-livenessStaleAfter - time.Minute), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
			if !tt.heartbeat.IsZero() {
				m.heartbeat.Store(tt.heartbeat.UnixNano())
			}
			rec := httptest.NewRecorder()
			m.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestTouchHeartbeat(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	before := time.Now().UnixNano()
	m.touchHeartbeat()
	if got := m.heartbeat.Load(); got < before {
		t.Errorf("heartbeat %d not updated (before %d)", got, before)
	}
}

// A worker pod stuck in Pending (unschedulable, image pull back-off, …) must
// release its images so they get dispatched again instead of being reserved
// in inProgress forever.
func TestCleanupFinishedWorkers_StuckPendingPodIsReleased(t *testing.T) {
	labels := map[string]string{"app": "oc-mirror-worker", "mirrortarget": "t"}
	stuck := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stuck", Namespace: "default", Labels: labels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-workerPendingTimeout - time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	fresh := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fresh", Namespace: "default", Labels: labels,
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	cs := k8sfake.NewSimpleClientset(stuck, fresh)
	m := NewWithClients(nil, cs, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{
		"d1": {Source: "s1", State: statePending},
		"d2": {Source: "s2", State: statePending},
	}
	m.owners = map[string][]string{"d1": {"is"}, "d2": {"is"}}
	m.inProgress = map[string]string{"d1": "stuck", "d2": "fresh"}

	m.cleanupFinishedWorkers(context.Background())

	if _, ok := m.inProgress["d1"]; ok {
		t.Error("expected d1 (stuck pod) to be released from inProgress")
	}
	if m.inProgress["d2"] != "fresh" {
		t.Error("expected d2 (freshly pending pod) to stay in progress")
	}
	if got := m.imageState["d1"].State; got != statePending {
		t.Errorf("d1 state = %q, want %q", got, statePending)
	}
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), "stuck", metav1.GetOptions{}); err == nil {
		t.Error("expected stuck pod to be deleted")
	}
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), "fresh", metav1.GetOptions{}); err != nil {
		t.Errorf("expected fresh pod to survive: %v", err)
	}
}

// A registry that never answers must not wedge the drift sweep: before the
// per-check timeout, driftSweepRunning stayed true forever and no later sweep
// ever re-detected images missing from the target registry.
func TestRunDriftSweep_HangingRegistryDoesNotWedgeSweep(t *testing.T) {
	orig := driftCheckTimeout
	driftCheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { driftCheckTimeout = orig })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	host := strings.TrimPrefix(srv.URL, "http://")

	m := newTestManagerForSignatureCheck(t, host)
	dest := host + "/example/img:v1"
	m.imageState = imagestate.ImageState{dest: {Source: "src", State: stateMirrored}}
	m.owners = map[string][]string{dest: {"is"}}
	m.driftSweepRunning = true

	done := make(chan struct{})
	go func() {
		m.runDriftSweep(context.Background(), []string{dest}, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("drift sweep did not finish against a hanging registry")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driftSweepRunning {
		t.Error("expected driftSweepRunning to be reset after the sweep")
	}
}

func TestHandleReadyz(t *testing.T) {
	tests := []struct {
		name      string
		listening bool
		heartbeat time.Time
		want      int
	}{
		{"status API not listening yet", false, time.Time{}, http.StatusServiceUnavailable},
		{"listening, before first heartbeat", true, time.Time{}, http.StatusOK},
		{"listening, recent heartbeat", true, time.Now(), http.StatusOK},
		{"listening, stalled reconcile loop", true, time.Now().Add(-livenessStaleAfter - time.Minute), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
			m.statusAPIReady.Store(tt.listening)
			if !tt.heartbeat.IsZero() {
				m.heartbeat.Store(tt.heartbeat.UnixNano())
			}
			rec := httptest.NewRecorder()
			m.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// The worker pod deadline is derived from the worker's own timeouts, rounded
// up to whole 5 minutes.
func TestWorkerImageBudget_DerivedFromWorkerTimeouts(t *testing.T) {
	if workerImageBudget < worker.ImageBudget {
		t.Fatalf("workerImageBudget %s below worker.ImageBudget %s", workerImageBudget, worker.ImageBudget)
	}
	if workerImageBudget != 45*time.Minute {
		t.Fatalf("workerImageBudget = %s, want 45m for the current worker timeouts", workerImageBudget)
	}
}
