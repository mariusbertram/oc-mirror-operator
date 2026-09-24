package manager

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// The flush writes MirrorTarget-level counts, deduplicated by destination and
// limited to ImageSets still in spec, for the MirrorTarget controller.
func TestFlush_WritesDeduplicatedSummary(t *testing.T) {
	m, _ := newReconcileTestManager(t, []string{"is-b", "is-a"}, "is-a", "is-b")
	m.imageState = imagestate.ImageState{
		"shared":  {Source: "s", State: stateMirrored},
		"only-a":  {Source: "a", State: statePending},
		"only-b":  {Source: "b", State: stateFailed, PermanentlyFailed: true},
		"removed": {Source: "r", State: statePending},
	}
	m.owners = map[string][]string{
		"shared":  {"is-a", "is-b"},
		"only-a":  {"is-a"},
		"only-b":  {"is-b"},
		"removed": {"is-gone"},
	}
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"is-b", "is-a"}},
	}
	ctx := context.Background()
	if err := m.flushPartitionedState(ctx, mt); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s, ok, err := imagestate.LoadSummary(ctx, m.Client, "default", "test")
	if err != nil || !ok {
		t.Fatalf("LoadSummary: ok=%v err=%v", ok, err)
	}
	if s.Total != 3 || s.Mirrored != 1 || s.Pending != 1 || s.Failed != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(s.ImageSets) != 2 || s.ImageSets[0] != "is-a" || s.ImageSets[1] != "is-b" {
		t.Fatalf("ImageSets = %v, want sorted [is-a is-b]", s.ImageSets)
	}
	if !m.summaryWritten {
		t.Fatal("summaryWritten not set")
	}
}

// A manager whose state never becomes dirty (settled target after an
// upgrade) still writes the summary once.
func TestReconcile_WritesSummaryWithoutDirtyState(t *testing.T) {
	m, _ := newReconcileTestManager(t, []string{"is-a"}, "is-a")
	m.imageState = imagestate.ImageState{"d": {Source: "s", State: stateMirrored}}
	m.owners = map[string][]string{"d": {"is-a"}}
	m.mirrored["d"] = true
	m.stateDirty = false

	ctx := context.Background()
	if err := m.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s, ok, err := imagestate.LoadSummary(ctx, m.Client, "default", "test")
	if err != nil || !ok {
		t.Fatalf("LoadSummary: ok=%v err=%v", ok, err)
	}
	if s.Total != 1 || s.Mirrored != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if !m.summaryWritten {
		t.Fatal("summaryWritten not set")
	}
}
