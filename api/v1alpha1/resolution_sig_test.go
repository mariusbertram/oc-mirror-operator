package v1alpha1

import (
	"testing"
)

func TestReleaseChannelSignature(t *testing.T) {
	rc1 := ReleaseChannel{
		Name:         "stable-4.14",
		Type:         TypeOCP,
		MinVersion:   "4.14.0",
		MaxVersion:   "4.14.5",
		Full:         false,
		ShortestPath: true,
	}

	rc2 := ReleaseChannel{
		Name:         "stable-4.14",
		Type:         TypeOCP,
		MinVersion:   "4.14.0",
		MaxVersion:   "4.14.5",
		Full:         false,
		ShortestPath: true,
	}

	arch1 := []string{"amd64", "arm64"}
	arch2 := []string{"arm64", "amd64"}

	// Stability and arch sorting check
	sig1 := ReleaseChannelSignature(rc1, arch1, true)
	sig2 := ReleaseChannelSignature(rc2, arch2, true)

	if sig1 != sig2 {
		t.Errorf("Expected signatures to match for identical semantic inputs:\nsig1: %s\nsig2: %s", sig1, sig2)
	}

	// Distinctness check: different arch
	sig3 := ReleaseChannelSignature(rc1, []string{"amd64"}, true)
	if sig1 == sig3 {
		t.Errorf("Expected different signature for different architectures")
	}

	// Distinctness check: different KubeVirt flag
	sig4 := ReleaseChannelSignature(rc1, arch1, false)
	if sig1 == sig4 {
		t.Errorf("Expected different signature for different KubeVirt flags")
	}

	// Distinctness check: different channel property
	rcDiff := rc1
	rcDiff.Name = "fast-4.14"
	sig5 := ReleaseChannelSignature(rcDiff, arch1, true)
	if sig1 == sig5 {
		t.Errorf("Expected different signature for different ReleaseChannel properties")
	}
}
