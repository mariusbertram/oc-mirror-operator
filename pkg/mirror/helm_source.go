package mirror

import (
	"fmt"
	"strings"
)

// helmChartSourceScheme prefixes the Source of an imagestate entry whose
// destination is a mirrored Helm chart archive (OriginHelmChart). The worker
// uses the origin to detect chart entries and the remainder of the reference
// to re-download the exact chart; a plain image copy would not work for
// chart repositories that serve the archive over plain HTTP(S) or an OCI
// artifact, neither of which is a container image.
const helmChartSourceScheme = "helm://"

// HelmChartSource builds the Source reference of a chart imagestate entry:
// helm://<repoURL>/<chartName>?version=<version>. repoURL may be an
// https:// chart repository or an oci:// registry namespace; the version is
// the version the chart reference resolved to (never empty).
func HelmChartSource(repoURL, chartName, version string) string {
	return fmt.Sprintf("%s%s/%s?version=%s", helmChartSourceScheme, strings.TrimSuffix(repoURL, "/"), chartName, version)
}

// ParseHelmChartSource splits a helm:// source reference back into its
// repository URL, chart name and resolved version. ok is false for any
// reference not produced by HelmChartSource.
func ParseHelmChartSource(source string) (repoURL, chartName, version string, ok bool) {
	if !strings.HasPrefix(source, helmChartSourceScheme) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(source, helmChartSourceScheme)
	chartName = rest
	if i := strings.Index(rest, "?version="); i >= 0 {
		chartName = rest[:i]
		version = rest[i+len("?version="):]
	}
	i := strings.LastIndex(chartName, "/")
	if i < 0 || i == len(chartName)-1 || version == "" {
		return "", "", "", false
	}
	repoURL = chartName[:i]
	chartName = chartName[i+1:]
	if repoURL == "" {
		return "", "", "", false
	}
	return repoURL, chartName, version, true
}

// IsChartSource reports whether src is a helm:// chart source reference
// produced by HelmChartSource.
func IsChartSource(src string) bool {
	return strings.HasPrefix(src, helmChartSourceScheme)
}

// HelmChartSignature builds the per-chart EntrySig used by the manager's
// cache carry-over: it changes whenever the mirrored chart's identity
// (repository, name, version) or its upstream archive content (digest)
// changes, so a changed upstream chart is re-queued instead of being carried
// over as already-mirrored.
func HelmChartSignature(repoName, chartName, version, archiveDigest string) string {
	return fmt.Sprintf("chart:%s/%s:%s@%s", repoName, chartName, version, archiveDigest)
}
