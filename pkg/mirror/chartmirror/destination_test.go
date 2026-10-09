package chartmirror

import "testing"

func TestChartDestinationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		registry string
	}{
		{"registry.example.com:5000"},
		{"registry.example.com:5000/mirror"}, // registry with repository path prefix
	} {
		dest := ChartDestination(tc.registry, "bitnami", "nginx", "15.5.1")
		registry, repoName, chart, version, err := ParseChartDestination(dest)
		if err != nil {
			t.Fatalf("ParseChartDestination(%q): %v", dest, err)
		}
		if registry != tc.registry || repoName != "bitnami" || chart != "nginx" || version != "15.5.1" {
			t.Errorf("round trip %q: got (%q, %q, %q, %q), want registry %q, bitnami, nginx, 15.5.1",
				dest, registry, repoName, chart, version, tc.registry)
		}
	}
}

func TestParseChartDestinationInvalid(t *testing.T) {
	for _, dest := range []string{
		"",
		"registry.example.com/charts/nginx:1.0.0",
		"registry.example.com/other/bitnami/nginx:1.0.0",
		"registry.example.com/charts/bitnami/nginx",
	} {
		if _, _, _, _, err := ParseChartDestination(dest); err == nil {
			t.Errorf("ParseChartDestination(%q) = nil error, want invalid", dest)
		}
	}
}
