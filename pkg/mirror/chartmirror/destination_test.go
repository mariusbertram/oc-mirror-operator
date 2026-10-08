package chartmirror

import "testing"

func TestChartDestinationRoundTrip(t *testing.T) {
	dest := ChartDestination("registry.example.com:5000", "bitnami", "nginx", "15.5.1")
	want := "registry.example.com:5000/charts/bitnami/nginx:15.5.1"
	if dest != want {
		t.Fatalf("ChartDestination = %q, want %q", dest, want)
	}
	registry, repoName, chart, version, err := ParseChartDestination(dest)
	if err != nil {
		t.Fatalf("ParseChartDestination: %v", err)
	}
	if registry != "registry.example.com:5000" || repoName != "bitnami" || chart != "nginx" || version != "15.5.1" {
		t.Errorf("got (%q, %q, %q, %q)", registry, repoName, chart, version)
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
