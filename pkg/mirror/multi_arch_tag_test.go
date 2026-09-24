package mirror

import "testing"

// The multi payload keeps oc-mirror v2's "<version>-multi" tag.
func TestReleaseTagFor_Multi(t *testing.T) {
	if got := releaseTagFor("4.16.10", "multi"); got != "4.16.10-multi" {
		t.Fatalf("releaseTagFor = %q", got)
	}
	if got := releasePayloadDestination("reg.io", releaseTagFor("4.16.10", "multi")); got != "reg.io/openshift/release-images:4.16.10-multi" {
		t.Fatalf("payload destination = %q", got)
	}
}
