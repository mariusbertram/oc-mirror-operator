package v1alpha1

import (
	"strings"
	"testing"
)

func TestCatalogDigestAnnotationKey(t *testing.T) {
	tests := []struct {
		name     string
		sig      string
		expected string
	}{
		{
			name:     "empty signature",
			sig:      "",
			expected: CatalogDigestAnnotationPrefix,
		},
		{
			name:     "short signature",
			sig:      "short",
			expected: CatalogDigestAnnotationPrefix + "short",
		},
		{
			name:     "exactly 48 chars signature",
			sig:      strings.Repeat("a", 48),
			expected: CatalogDigestAnnotationPrefix + strings.Repeat("a", 48),
		},
		{
			name:     "longer than 48 chars signature",
			sig:      strings.Repeat("a", 48) + "extra",
			expected: CatalogDigestAnnotationPrefix + strings.Repeat("a", 48),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CatalogDigestAnnotationKey(tt.sig)
			if result != tt.expected {
				t.Errorf("CatalogDigestAnnotationKey(%q) = %q, expected %q", tt.sig, result, tt.expected)
			}
		})
	}
}

func TestReleaseDigestAnnotationKey(t *testing.T) {
	tests := []struct {
		name     string
		sig      string
		expected string
	}{
		{
			name:     "empty signature",
			sig:      "",
			expected: ReleaseDigestAnnotationPrefix,
		},
		{
			name:     "short signature",
			sig:      "short",
			expected: ReleaseDigestAnnotationPrefix + "short",
		},
		{
			name:     "exactly 48 chars signature",
			sig:      strings.Repeat("a", 48),
			expected: ReleaseDigestAnnotationPrefix + strings.Repeat("a", 48),
		},
		{
			name:     "longer than 48 chars signature",
			sig:      strings.Repeat("a", 48) + "extra",
			expected: ReleaseDigestAnnotationPrefix + strings.Repeat("a", 48),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReleaseDigestAnnotationKey(tt.sig)
			if result != tt.expected {
				t.Errorf("ReleaseDigestAnnotationKey(%q) = %q, expected %q", tt.sig, result, tt.expected)
			}
		})
	}
}
