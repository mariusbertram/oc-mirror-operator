package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func newQueueTestManager(cs *k8sfake.Clientset) *MirrorManager {
	if cs == nil {
		cs = k8sfake.NewSimpleClientset()
	}
	m := NewWithClients(nil, cs, "t", "default", "img", "", runtime.NewScheme())
	m.workerToken = "tok"
	m.imageState = imagestate.ImageState{
		"d1": {Source: "s1", State: statePending},
		"d2": {Source: "s2", State: statePending},
	}
	m.owners = map[string][]string{"d1": {"is"}, "d2": {"is"}}
	m.inProgress = map[string]string{"d1": "w1", "d2": "w1"}
	return m
}

func postStatus(m *MirrorManager, req WorkerStatusRequest) int {
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/status", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	m.handleStatusUpdate(rr, r)
	return rr.Code
}

// A reconcile tick blocked on a slow API server holds m.mu for its whole
// flush. A worker's status report must still be acknowledged immediately
// and applied once the tick releases the lock — not time out and get lost.
func TestStatusUpdate_NotBlockedWhileReconcileHoldsLock(t *testing.T) {
	m := newQueueTestManager(nil)

	m.mu.Lock() // simulate a reconcile tick stuck in flushPartitionedState
	done := make(chan int, 1)
	go func() { done <- postStatus(m, WorkerStatusRequest{PodName: "w1", Destination: "d1"}) }()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status code = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		m.mu.Unlock()
		t.Fatal("status callback blocked on m.mu")
	}
	if got := m.imageState["d1"].State; got != statePending {
		t.Fatalf("state applied while lock held: %s", got)
	}
	select {
	case <-m.urgentFlush:
	default:
		t.Fatal("urgentFlush not signalled")
	}

	m.drainStatusQueueLocked() // what the next reconcile does first
	m.mu.Unlock()

	if got := m.imageState["d1"].State; got != stateMirrored {
		t.Fatalf("state after drain = %s, want Mirrored", got)
	}
	if _, ok := m.inProgress["d1"]; ok {
		t.Fatal("d1 still in progress after drain")
	}
	if len(m.statusQueue) != 0 {
		t.Fatalf("queue not empty: %v", m.statusQueue)
	}
}

// With the lock free the report is applied synchronously, as before.
func TestStatusUpdate_AppliedImmediatelyWhenLockFree(t *testing.T) {
	m := newQueueTestManager(nil)
	if code := postStatus(m, WorkerStatusRequest{PodName: "w1", Destination: "d1", Error: "boom"}); code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	e := m.imageState["d1"]
	if e.State != stateFailed || e.RetryCount != 1 || e.LastError != "boom" {
		t.Fatalf("entry = %+v", e)
	}
	if len(m.statusQueue) != 0 {
		t.Fatal("report left in queue")
	}
}

// Reports are applied in arrival order.
func TestDrainStatusQueue_Order(t *testing.T) {
	m := newQueueTestManager(nil)
	m.enqueueStatus(WorkerStatusRequest{Destination: "d1", Error: "first"})
	m.enqueueStatus(WorkerStatusRequest{Destination: "d1"})
	m.mu.Lock()
	m.drainStatusQueueLocked()
	m.mu.Unlock()
	if got := m.imageState["d1"].State; got != stateMirrored {
		t.Fatalf("state = %s, want Mirrored (last report wins)", got)
	}
}

// A worker pod that reported all its images and then exited must not have
// those images reset or re-dispatched just because the reports are still
// queued when cleanupFinishedWorkers sees the pod as finished.
func TestCleanupFinishedWorkers_DrainsQueueFirst(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "w1", Namespace: "default",
			Labels: map[string]string{"app": "oc-mirror-worker", "mirrortarget": "t"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	m := newQueueTestManager(k8sfake.NewSimpleClientset(pod))
	m.enqueueStatus(WorkerStatusRequest{PodName: "w1", Destination: "d1"})

	m.cleanupFinishedWorkers(context.Background())

	if got := m.imageState["d1"].State; got != stateMirrored {
		t.Fatalf("d1 = %s, want Mirrored from the queued report", got)
	}
	// d2 had no report: the failed pod's image goes back to Pending.
	if got := m.imageState["d2"].State; got != statePending {
		t.Fatalf("d2 = %s, want Pending", got)
	}
	if len(m.inProgress) != 0 {
		t.Fatalf("inProgress = %v", m.inProgress)
	}
}

func getShouldMirror(m *MirrorManager, dest string) int {
	r := httptest.NewRequest(http.MethodGet, "/should-mirror?dest="+dest, nil)
	r.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	m.handleShouldMirror(rr, r)
	return rr.Code
}

func TestShouldMirror_WhileLockHeld(t *testing.T) {
	m := newQueueTestManager(nil)
	m.enqueueStatus(WorkerStatusRequest{Destination: "d1"})

	m.mu.Lock()
	defer m.mu.Unlock()

	// A queued success is answered from the queue without the lock.
	if code := getShouldMirror(m, "d1"); code != http.StatusGone {
		t.Fatalf("d1 = %d, want 410", code)
	}
	// Otherwise the handler fails open after shouldMirrorLockWait.
	start := time.Now()
	if code := getShouldMirror(m, "d2"); code != http.StatusOK {
		t.Fatalf("d2 = %d, want 200", code)
	}
	if elapsed := time.Since(start); elapsed < shouldMirrorLockWait || elapsed > shouldMirrorLockWait+time.Second {
		t.Fatalf("waited %s, want about %s", elapsed, shouldMirrorLockWait)
	}
}

func TestShouldMirror_ReadLockCoexistsWithReaders(t *testing.T) {
	m := newQueueTestManager(nil)
	m.imageState["d1"].State = stateMirrored
	m.mu.RLock()
	defer m.mu.RUnlock()
	if code := getShouldMirror(m, "d1"); code != http.StatusGone {
		t.Fatalf("d1 = %d, want 410", code)
	}
}
