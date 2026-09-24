package manager

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func TestRetryBackoff(t *testing.T) {
	tests := []struct {
		retryCount int
		want       time.Duration
	}{
		{0, retryBackoffMax}, {1, time.Minute}, {2, 2 * time.Minute}, {3, 4 * time.Minute},
		{6, 32 * time.Minute}, {7, retryBackoffMax}, {10, retryBackoffMax}, {100, retryBackoffMax},
	}
	for _, tt := range tests {
		for i := 0; i < 50; i++ { // jitter is random: check the bounds repeatedly
			got := retryBackoff(tt.retryCount)
			if lo, hi := tt.want-tt.want/10, tt.want+tt.want/10; got < lo || got > hi {
				t.Fatalf("retryBackoff(%d) = %s, want within ±10%% of %s", tt.retryCount, got, tt.want)
			}
		}
	}
}

// A failed image waits out its backoff instead of being retried on the next
// tick, and uses the configured retry budget (#144).
func TestFailedImage_BacksOffAndUsesMaxRetries(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	m.maxRetries = 3
	m.imageState = imagestate.ImageState{"d": {Source: "s", State: statePending}}

	before := time.Now()
	m.setImageStateLocked("d", stateFailed, "upstream 503")
	e := m.imageState["d"]
	if e.NextRetryAt == nil || !e.NextRetryAt.After(before) {
		t.Fatalf("NextRetryAt = %v, want a time in the future", e.NextRetryAt)
	}
	if e.PermanentlyFailed {
		t.Fatal("must not be permanently failed after the first failure")
	}

	for i := 0; i < 2; i++ {
		e.State = statePending
		m.setImageStateLocked("d", stateFailed, "upstream 503 again")
		e.LastError = "" // allow the next identical report to count
	}
	if e.RetryCount != 3 || !e.PermanentlyFailed {
		t.Errorf("RetryCount=%d PermanentlyFailed=%v, want 3/true with maxRetries=3", e.RetryCount, e.PermanentlyFailed)
	}

	m.setImageStateLocked("d", statePending, "")
	if e.NextRetryAt != nil {
		t.Error("NextRetryAt must be cleared when the entry leaves the Failed state")
	}
}

func TestReconcile_RetriesFailedImageOnlyAfterBackoff(t *testing.T) {
	m, cs := newReconcileTestManager(t, []string{"is"}, "is")
	future := metav1.NewTime(time.Now().Add(10 * time.Minute))
	m.imageState = imagestate.ImageState{"reg.io/a:v1": {Source: "quay.io/a:v1", State: stateFailed, RetryCount: 2, NextRetryAt: &future}}
	m.owners = map[string][]string{"reg.io/a:v1": {"is"}}

	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.imageState["reg.io/a:v1"].State; got != stateFailed || countWorkerPods(t, cs) != 0 {
		t.Fatalf("state %s, %d pods: an image in backoff must not be retried", got, countWorkerPods(t, cs))
	}

	past := metav1.NewTime(time.Now().Add(-time.Second))
	m.imageState["reg.io/a:v1"].NextRetryAt = &past
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := m.imageState["reg.io/a:v1"]
	if e.State != statePending || e.NextRetryAt != nil {
		t.Fatalf("state %s, NextRetryAt %v: want re-queued once the backoff passed", e.State, e.NextRetryAt)
	}
	// Re-queued entries are dispatched on the following tick, as before.
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countWorkerPods(t, cs); n != 1 {
		t.Errorf("%d worker pods, want the image dispatched", n)
	}
}
