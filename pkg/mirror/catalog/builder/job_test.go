/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package builder

import (
	"strings"
	"testing"
)

// TestSafeJobName_DNSCompliant verifies that all generated names fit in 63
// characters, are lower-case, and do not end with a dash.
func TestSafeJobName_DNSCompliant(t *testing.T) {
	cases := [][]string{
		{"is-1", "quay.io/some/catalog:tag"},
		{"a-very-long-imageset-name-that-keeps-going-and-going", "registry.example.com/very/deep/path/catalog:v4.21.0"},
		{strings.Repeat("x", 200), strings.Repeat("y", 200)},
		{"is", "img"},
	}
	for _, c := range cases {
		got := safeJobName("catalog-build", c...)
		if len(got) > 63 {
			t.Errorf("name %q too long (%d)", got, len(got))
		}
		if strings.ToLower(got) != got {
			t.Errorf("name %q not lower-case", got)
		}
		if strings.HasSuffix(got, "-") {
			t.Errorf("name %q ends with dash", got)
		}
	}
}

// TestSafeJobName_NoCollisionViaTruncation verifies that two long inputs
// differing only in their suffix do not collide after truncation, because
// the SHA-256-derived 8-char hash discriminates them.
func TestSafeJobName_NoCollisionViaTruncation(t *testing.T) {
	a := safeJobName("catalog-build", "is", strings.Repeat("a", 80)+"X")
	b := safeJobName("catalog-build", "is", strings.Repeat("a", 80)+"Y")
	if a == b {
		t.Fatalf("expected different names for differing inputs, got %q == %q", a, b)
	}
}

// TestNew_RequiresOperatorImage ensures we fail fast when OPERATOR_IMAGE is
// unset rather than launching catalog-build Jobs with a bogus image.
func TestNew_RequiresOperatorImage(t *testing.T) {
	t.Setenv(OperatorImageEnvVar, "")
	_, err := New()
	if err == nil {
		t.Fatalf("expected error when OPERATOR_IMAGE is empty")
	}
	if !strings.Contains(err.Error(), OperatorImageEnvVar) {
		t.Fatalf("error %q does not mention env var name", err)
	}
}

func TestNew_AcceptsOperatorImage(t *testing.T) {
	t.Setenv(OperatorImageEnvVar, "registry.example.com/operator:v1")
	m, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatalf("expected manager, got nil")
	}
}
