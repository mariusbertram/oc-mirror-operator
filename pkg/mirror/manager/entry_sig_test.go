package manager

import (
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// After a spec edit changes an operator entry's signature (S1 → S2), the
// existing destinations must carry S2, otherwise the next cache-hit resolve
// drops them and orphans still-needed images (#130).
func TestMergeResolvedIntoConsolidated_RefreshesSpecMetadata(t *testing.T) {
	const dest = "reg.io/a:sha256-1"
	state := imagestate.ImageState{dest: {
		Source: "src", Origin: imagestate.OriginOperator, EntrySig: "S1", OriginRef: "old",
		State: stateFailed, RetryCount: 4, LastError: "boom", SourceDigest: "sha256:x", SignatureVerified: true,
	}}
	owners := map[string][]string{dest: {"my-is"}}
	resolved := imagestate.ImageState{dest: {
		Source: "src", Origin: imagestate.OriginOperator, EntrySig: "S2", OriginRef: "new", IsBundleImage: true, State: statePending,
	}}

	if orphaned := mergeResolvedIntoConsolidated(state, owners, resolved, "my-is"); len(orphaned) != 0 {
		t.Fatalf("unexpected orphans: %v", orphaned)
	}

	got := state[dest]
	if got.EntrySig != "S2" || got.OriginRef != "new" || !got.IsBundleImage {
		t.Errorf("spec metadata not refreshed: sig=%q ref=%q bundle=%v", got.EntrySig, got.OriginRef, got.IsBundleImage)
	}
	if got.State != stateFailed || got.RetryCount != 4 || got.LastError != "boom" || got.SourceDigest != "sha256:x" || !got.SignatureVerified {
		t.Errorf("lifecycle fields must be preserved, got %+v", *got)
	}

	// The next cache-hit resolve (carry-over by the new signature) keeps it.
	next := imagestate.ImageState{}
	carryOverByOriginAndSig(filterByImageSet(state, owners, nil, "my-is"), next, imagestate.OriginOperator, "S2", "new")
	if _, ok := next[dest]; !ok {
		t.Error("destination dropped by cache-hit carry-over after spec edit")
	}
}

// A destination shared with another ImageSet keeps its metadata — the entry
// is single-valued and the other owner's resolve may have written it.
func TestMergeResolvedIntoConsolidated_SharedKeepsMetadata(t *testing.T) {
	const dest = "reg.io/a:sha256-1"
	state := imagestate.ImageState{dest: {Source: "src", Origin: imagestate.OriginOperator, EntrySig: "S-other", State: stateMirrored}}
	owners := map[string][]string{dest: {"other-is", "my-is"}}
	resolved := imagestate.ImageState{dest: {Source: "src2", Origin: imagestate.OriginOperator, EntrySig: "S2", State: statePending}}

	mergeResolvedIntoConsolidated(state, owners, resolved, "my-is")

	if got := state[dest]; got.EntrySig != "S-other" || got.Source != "src2" || got.State != stateMirrored {
		t.Errorf("shared entry: got sig=%q source=%q state=%q", got.EntrySig, got.Source, got.State)
	}
}
