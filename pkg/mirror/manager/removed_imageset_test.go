package manager

import (
	"context"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// Images of an ImageSet removed from spec.imageSets must no longer be
// dispatched, while images it shared with a remaining ImageSet stay (#131).
func TestReconcile_DropsRemovedImageSet(t *testing.T) {
	m, cs := newReconcileTestManager(t, []string{"kept-is"}, "kept-is", "removed-is")
	const exclusive, shared = "reg.io/removed:v1", "reg.io/shared:v1"
	m.imageState = imagestate.ImageState{
		exclusive: {Source: "quay.io/removed:v1", State: statePending},
		shared:    {Source: "quay.io/shared:v1", State: stateMirrored},
	}
	m.owners = map[string][]string{
		exclusive: {"removed-is"},
		shared:    {"kept-is", "removed-is"},
	}
	m.catalogDigests = map[string]map[string]string{"removed-is": {"k": "v"}, "kept-is": {"k": "v"}}

	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := countWorkerPods(t, cs); n != 0 {
		t.Errorf("expected no worker for the removed ImageSet, got %d pod(s)", n)
	}
	if _, ok := m.imageState[exclusive]; ok {
		t.Error("expected the removed ImageSet's exclusive entry to be dropped")
	}
	if got := m.owners[shared]; len(got) != 1 || got[0] != "kept-is" {
		t.Errorf("shared owners = %v, want [kept-is]", got)
	}
	if _, ok := m.catalogDigests["removed-is"]; ok {
		t.Error("expected the removed ImageSet's catalog digests to be forgotten")
	}

	// Dropped entries are not staged as orphans — the MirrorTarget
	// controller cleans up removed ImageSets itself.
	orphans, err := imagestate.LoadByConfigMapName(context.Background(), m.Client, "default", imagestate.OrphansConfigMapName("test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("expected no orphans, got %v", orphans)
	}
}
