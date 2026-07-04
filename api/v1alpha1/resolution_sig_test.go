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

func TestOperatorEntrySignature(t *testing.T) {
	// 1. Identical Operator inputs producing the same signature
	op1 := Operator{
		Catalog: "registry.example.com/catalog:latest",
		IncludeConfig: IncludeConfig{
			Packages: []IncludePackage{
				{Name: "pkg1"},
			},
		},
	}

	sig1 := OperatorEntrySignature(op1)
	sig2 := OperatorEntrySignature(op1)
	if sig1 != sig2 {
		t.Errorf("Expected identical signatures for identical operators, got %s and %s", sig1, sig2)
	}

	// 2. Operator inputs with different package orders producing the same signature
	op2 := Operator{
		Catalog: "registry.example.com/catalog:latest",
		IncludeConfig: IncludeConfig{
			Packages: []IncludePackage{
				{Name: "pkg1"},
				{Name: "pkg2"},
			},
		},
	}
	op3 := Operator{
		Catalog: "registry.example.com/catalog:latest",
		IncludeConfig: IncludeConfig{
			Packages: []IncludePackage{
				{Name: "pkg2"},
				{Name: "pkg1"},
			},
		},
	}
	if OperatorEntrySignature(op2) != OperatorEntrySignature(op3) {
		t.Errorf("Expected package order to not affect signature")
	}

	// 3. Operator inputs with different channel orders within a package producing the same signature
	op4 := Operator{
		Catalog: "registry.example.com/catalog:latest",
		IncludeConfig: IncludeConfig{
			Packages: []IncludePackage{
				{
					Name: "pkg1",
					Channels: []IncludeChannel{
						{Name: "chan1"},
						{Name: "chan2"},
					},
				},
			},
		},
	}
	op5 := Operator{
		Catalog: "registry.example.com/catalog:latest",
		IncludeConfig: IncludeConfig{
			Packages: []IncludePackage{
				{
					Name: "pkg1",
					Channels: []IncludeChannel{
						{Name: "chan2"},
						{Name: "chan1"},
					},
				},
			},
		},
	}
	if OperatorEntrySignature(op4) != OperatorEntrySignature(op5) {
		t.Errorf("Expected channel order to not affect signature")
	}

	// 4. Variations in structural fields correctly impact signature
	baseOp := Operator{Catalog: "catalog:latest"}
	baseSig := OperatorEntrySignature(baseOp)

	variations := []struct {
		name        string
		op          Operator
		shouldEqual bool
	}{
		// Changes to hashing fields should affect signature
		{"different catalog", Operator{Catalog: "catalog:other"}, false},
		{"full true", Operator{Catalog: "catalog:latest", Full: true}, false},
		{"skip dependencies true", Operator{Catalog: "catalog:latest", SkipDependencies: true}, false},

		// Target-related fields ARE included in the hash in OperatorEntrySignature payload struct,
		// so they do affect the signature in current implementation.
		{"target catalog", Operator{Catalog: "catalog:latest", TargetCatalog: "target"}, false},
		{"target tag", Operator{Catalog: "catalog:latest", TargetTag: "v1"}, false},
	}

	for _, v := range variations {
		t.Run(v.name, func(t *testing.T) {
			equal := OperatorEntrySignature(v.op) == baseSig
			if equal != v.shouldEqual {
				t.Errorf("Variation %q expected equality %v, got %v", v.name, v.shouldEqual, equal)
			}
		})
	}
}

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
