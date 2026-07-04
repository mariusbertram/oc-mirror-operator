/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"strings"
	"testing"
)

func TestCatalogDigestAnnotationKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short string",
			input:    "12345",
			expected: CatalogDigestAnnotationPrefix + "12345",
		},
		{
			name:     "exact 48 chars",
			input:    strings.Repeat("a", 48),
			expected: CatalogDigestAnnotationPrefix + strings.Repeat("a", 48),
		},
		{
			name:     "long string (> 48 chars)",
			input:    strings.Repeat("b", 50),
			expected: CatalogDigestAnnotationPrefix + strings.Repeat("b", 48),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CatalogDigestAnnotationKey(tt.input)
			if result != tt.expected {
				t.Errorf("CatalogDigestAnnotationKey(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestReleaseDigestAnnotationKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short string",
			input:    "abcde",
			expected: ReleaseDigestAnnotationPrefix + "abcde",
		},
		{
			name:     "exact 48 chars",
			input:    strings.Repeat("x", 48),
			expected: ReleaseDigestAnnotationPrefix + strings.Repeat("x", 48),
		},
		{
			name:     "long string (> 48 chars)",
			input:    strings.Repeat("y", 60),
			expected: ReleaseDigestAnnotationPrefix + strings.Repeat("y", 48),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReleaseDigestAnnotationKey(tt.input)
			if result != tt.expected {
				t.Errorf("ReleaseDigestAnnotationKey(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

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
