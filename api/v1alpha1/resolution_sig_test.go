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
