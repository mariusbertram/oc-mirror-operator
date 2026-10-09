package mirror

import (
	"fmt"
	"net/url"
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
// the version the chart reference resolved to (never empty). Both the chart
// name and the version are query-escaped so special characters (?, #, %)
// cannot corrupt the reference, and userinfo embedded in repoURL (e.g.
// https://user:pass@repo/) is stripped: the Source is persisted in the
// imagestate ConfigMap and echoed in worker logs, where credentials must
// never appear.
func HelmChartSource(repoURL, chartName, version string) string {
	return fmt.Sprintf("%s%s/%s?version=%s", helmChartSourceScheme, strings.TrimSuffix(redactURL(repoURL), "/"), url.QueryEscape(chartName), url.QueryEscape(version))
}

// ParseHelmChartSource splits a helm:// source reference back into its
// repository URL, chart name and resolved version. ok is false for any
// reference not produced by HelmChartSource.
func ParseHelmChartSource(source string) (repoURL, chartName, version string, ok bool) {
	if !strings.HasPrefix(source, helmChartSourceScheme) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(source, helmChartSourceScheme)
	if i := strings.Index(rest, "?version="); i >= 0 {
		var err error
		version, err = url.QueryUnescape(rest[i+len("?version="):])
		if err != nil || version == "" {
			return "", "", "", false
		}
		rest = rest[:i]
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 || i == 0 || i == len(rest)-1 {
		return "", "", "", false
	}
	repoURL = rest[:i]
	var err error
	chartName, err = url.QueryUnescape(rest[i+1:])
	if err != nil || chartName == "" || repoURL == "" || version == "" {
		return "", "", "", false
	}
	return repoURL, chartName, version, true
}

// redactURL strips any userinfo (user:password@) from a URL so credentials
// never reach the imagestate ConfigMap or worker logs. A URL without
// userinfo, or one that cannot be parsed, is returned unchanged.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	redacted := *u
	redacted.User = nil
	return redacted.String()
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
