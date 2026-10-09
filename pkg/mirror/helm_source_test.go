package mirror

import (
	"strings"
	"testing"
)

func TestHelmChartSourceRoundTrip(t *testing.T) {
	cases := []struct {
		repoURL, chart, version string
	}{
		{"https://charts.bitnami.com/bitnami", "nginx", "15.5.1"},
		{"oci://registry-1.docker.io/bitnamicharts", "redis", "18.6.1"},
		{"https://repo.example.com/charts/", "mychart", "1.0.0"}, // trailing slash
	}
	for _, tc := range cases {
		src := HelmChartSource(tc.repoURL, tc.chart, tc.version)
		repoURL, chart, version, ok := ParseHelmChartSource(src)
		if !ok {
			t.Errorf("ParseHelmChartSource(%q) not ok", src)
			continue
		}
		wantRepo := tc.repoURL
		if wantRepo[len(wantRepo)-1] == '/' {
			wantRepo = wantRepo[:len(wantRepo)-1]
		}
		if repoURL != wantRepo || chart != tc.chart || version != tc.version {
			t.Errorf("round trip %q: got (%q, %q, %q), want (%q, %q, %q)",
				src, repoURL, chart, version, wantRepo, tc.chart, tc.version)
		}
		if !IsChartSource(src) {
			t.Errorf("IsChartSource(%q) = false, want true", src)
		}
	}
}

func TestParseHelmChartSourceInvalid(t *testing.T) {
	for _, src := range []string{
		"",
		"quay.io/foo/bar:latest",
		"helm://onlyrepo",
		"helm://repo/chart",
		"helm://",
	} {
		if _, _, _, ok := ParseHelmChartSource(src); ok {
			t.Errorf("ParseHelmChartSource(%q) ok, want invalid", src)
		}
	}
	// IsChartSource is a prefix check only; malformed helm:// references
	// are still routed to the chart copier, which rejects them with a
	// clear error via ParseHelmChartSource.
	for _, src := range []string{"", "quay.io/foo/bar:latest"} {
		if IsChartSource(src) {
			t.Errorf("IsChartSource(%q) = true, want false", src)
		}
	}
}

func TestHelmChartSourceEscapesSpecialChars(t *testing.T) {
	src := HelmChartSource("https://charts.example.com", "my?chart", "1.0.0+build#1")
	repoURL, chart, version, ok := ParseHelmChartSource(src)
	if !ok {
		t.Fatalf("ParseHelmChartSource(%q) not ok", src)
	}
	if repoURL != "https://charts.example.com" || chart != "my?chart" || version != "1.0.0+build#1" {
		t.Errorf("got (%q, %q, %q)", repoURL, chart, version)
	}
}

func TestHelmChartSourceRedactsUserinfo(t *testing.T) {
	src := HelmChartSource("https://user:secret@charts.example.com/charts", "mychart", "1.0.0")
	if strings.Contains(src, "user") || strings.Contains(src, "secret") {
		t.Errorf("source %q leaks credentials", src)
	}
	repoURL, _, _, ok := ParseHelmChartSource(src)
	if !ok {
		t.Fatalf("ParseHelmChartSource(%q) not ok", src)
	}
	if repoURL != "https://charts.example.com/charts" {
		t.Errorf("repoURL = %q, want userinfo-stripped URL", repoURL)
	}
}

func TestHelmChartSignature(t *testing.T) {
	a := HelmChartSignature("bitnami", "nginx", "15.5.1", "sha256:abc")
	b := HelmChartSignature("bitnami", "nginx", "15.5.1", "sha256:def")
	c := HelmChartSignature("bitnami", "nginx", "15.5.2", "sha256:abc")
	if a == b || a == c {
		t.Error("signature must change with archive digest and version")
	}
	if a != HelmChartSignature("bitnami", "nginx", "15.5.1", "sha256:abc") {
		t.Error("signature must be deterministic")
	}
}
