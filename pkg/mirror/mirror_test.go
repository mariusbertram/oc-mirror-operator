package mirror

import (
	"testing"
)

func TestDigestToTag(t *testing.T) {
	// We want to test the logic that converts a digest to a tag when no tag is present
	// Source: registry/repo@sha256:abc... -> Destination: target/repo:sha256-abc...

	// src := "quay.io/oc-mirror/test@sha256:45b41094054a1656837966006c9a3099991234567890abcdef1234567890abcd"
	// destBase := "internal.registry.com/mirror/test"
}

func TestCheckExist(t *testing.T) {
}
