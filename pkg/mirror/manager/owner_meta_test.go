package manager

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

const sharedDest = "reg.io/bundle:sha256-1"

// resolveInto applies what reconcile() does after a successful resolve of
// isName that produced dest with the given signature.
func resolveInto(m *MirrorManager, isName, sig string) {
	resolved := imagestate.ImageState{sharedDest: {
		Source: "quay.io/bundle@sha256:1", Origin: imagestate.OriginOperator, EntrySig: sig, OriginRef: "ref-" + isName, State: statePending,
	}}
	mergeResolvedIntoConsolidated(m.imageState, m.owners, resolved, isName)
	m.recordOwnerMetaLocked(isName, resolved)
}

// Two ImageSets resolving the same destination from different spec entries
// must each keep it on their next cache hit (#160).
func TestOwnerMeta_SharedDestinationSurvivesEachOwnersCacheHit(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	resolveInto(m, "is-a", "sigA")
	resolveInto(m, "is-b", "sigB")

	for isName, sig := range map[string]string{"is-a": "sigA", "is-b": "sigB"} {
		view := m.filterByImageSetLocked(isName)
		if got := view[sharedDest].EntrySig; got != sig {
			t.Fatalf("%s view EntrySig = %q, want %q", isName, got, sig)
		}
		next := imagestate.ImageState{}
		carryOverByOriginAndSig(view, next, imagestate.OriginOperator, sig, "ref")
		if _, kept := next[sharedDest]; !kept {
			t.Errorf("%s: cache-hit carry-over dropped the shared destination", isName)
		}
	}
}

// Each owner's state ConfigMap carries its own metadata, and a cold start
// (manager restart) restores it per owner.
func TestOwnerMeta_FlushAndLoadRoundTrip(t *testing.T) {
	m, _ := newReconcileTestManager(t, []string{"is-a", "is-b"}, "is-a", "is-b")
	resolveInto(m, "is-a", "sigA")
	resolveInto(m, "is-b", "sigB")
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"is-a", "is-b"}},
	}
	ctx := context.Background()
	if err := m.flushPartitionedState(ctx, mt); err != nil {
		t.Fatal(err)
	}

	for isName, sig := range map[string]string{"is-a": "sigA", "is-b": "sigB"} {
		st, err := imagestate.Load(ctx, m.Client, "default", isName)
		if err != nil {
			t.Fatal(err)
		}
		if got := st[sharedDest].EntrySig; got != sig {
			t.Errorf("%s ConfigMap EntrySig = %q, want %q", isName, got, sig)
		}
	}

	restarted := NewWithClients(m.Client, nil, "test", "default", "img", "", m.Scheme)
	list := &mirrorv1alpha1.ImageSetList{}
	if err := m.Client.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	restarted.loadPartitionedState(ctx, mt, list)
	for isName, sig := range map[string]string{"is-a": "sigA", "is-b": "sigB"} {
		if got := restarted.filterByImageSetLocked(isName)[sharedDest].EntrySig; got != sig {
			t.Errorf("after restart %s view EntrySig = %q, want %q", isName, got, sig)
		}
	}
}

// Metadata is forgotten together with ownership.
func TestOwnerMeta_ForgottenWithOwnership(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	resolveInto(m, "is-a", "sigA")
	resolveInto(m, "is-b", "sigB")

	// is-b no longer resolves the destination.
	mergeResolvedIntoConsolidated(m.imageState, m.owners, imagestate.ImageState{}, "is-b")
	m.recordOwnerMetaLocked("is-b", imagestate.ImageState{})
	if _, ok := m.ownerMeta[sharedDest]["is-b"]; ok {
		t.Error("expected is-b's metadata to be removed with its ownership")
	}

	// is-a is removed from the MirrorTarget: the destination is dropped.
	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"other"}}}
	m.dropRemovedImageSetsLocked(mt)
	if _, ok := m.ownerMeta[sharedDest]; ok {
		t.Error("expected all metadata of a dropped destination to be removed")
	}
}

func TestOwnerMeta_PrunedForRemovedOwnerOfSharedDestination(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	resolveInto(m, "is-a", "sigA")
	resolveInto(m, "is-b", "sigB")

	mt := &mirrorv1alpha1.MirrorTarget{Spec: mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"is-a"}}}
	m.dropRemovedImageSetsLocked(mt)

	if _, ok := m.ownerMeta[sharedDest]["is-b"]; ok {
		t.Error("expected the removed owner's metadata to be pruned")
	}
	if got := m.ownerMeta[sharedDest]["is-a"].EntrySig; got != "sigA" {
		t.Errorf("remaining owner's metadata = %q, want sigA", got)
	}
}
