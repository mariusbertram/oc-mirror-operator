package v1alpha1

import (
	"slices"
	"testing"
)

func TestACSScannerDBBundleVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   ACSScannerDB
		want []string
	}{
		{"legacy", ACSScannerDB{}, nil},
		{"single", ACSScannerDB{Version: "4.11.0"}, []string{"4.11.0"}},
		{"list", ACSScannerDB{Versions: []string{"4.10.3", "4.11.0"}}, []string{"4.10.3", "4.11.0"}},
		{"combined without duplicates", ACSScannerDB{Version: "4.11.0", Versions: []string{"4.10.3", "4.11.0"}}, []string{"4.11.0", "4.10.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.db.BundleVersions(); !slices.Equal(got, tc.want) {
				t.Errorf("BundleVersions() = %v, want %v", got, tc.want)
			}
		})
	}
}
