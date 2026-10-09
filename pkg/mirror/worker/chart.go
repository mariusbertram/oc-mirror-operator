package worker

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"helm.sh/helm/v4/pkg/chart/v2/loader"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/chartmirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/helm"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

// ChartMirrorTimeout bounds a full chart mirror (download + push), the chart
// counterpart of CopyTimeout for images.
const ChartMirrorTimeout = 10 * time.Minute

// MirrorChart mirrors a single Helm chart archive into the target registry
// as a Helm OCI artifact: it downloads the chart from its repository via the
// same resolver the manager used to resolve it (same index resolution and
// oci:// pull path), then pushes the archive with the worker's own registry
// client (DOCKER_CONFIG credentials, insecure-host handling) — matching how
// images are copied. The manager only queues the entry; every registry push
// happens here, in the worker. Returns the pushed destination reference.
//
// Note: like the manager's resolver, the registry client is configured for
// the DESTINATION host (insecure via the MirrorTarget's spec.insecure). A
// chart stored in an OCI registry that itself requires TLS-skip is not
// supported for oci:// sources — only HTTP(S) chart repositories take
// that path today.
func MirrorChart(ctx context.Context, c *mirrorclient.MirrorClient, source, dest string) (string, error) {
	repoURL, chartName, version, ok := mirror.ParseHelmChartSource(source)
	if !ok {
		return "", fmt.Errorf("invalid helm chart source %q", source)
	}

	resolver := helm.NewWithOCI(c)
	archive, resolvedVersion, err := resolver.DownloadChartArchive(ctx, repoURL, mirrorv1alpha1.Chart{Name: chartName, Version: version})
	if err != nil {
		return "", fmt.Errorf("download chart %s/%s: %w", repoURL, chartName, err)
	}
	if resolvedVersion != version {
		return "", fmt.Errorf("chart %s/%s resolved to version %q, expected %q", repoURL, chartName, resolvedVersion, version)
	}

	// Validate the archive really is a chart before pushing, so a
	// misconfigured repository serving an HTML error page fails loudly
	// instead of pushing garbage.
	if _, err := loader.LoadArchive(bytes.NewReader(archive)); err != nil {
		return "", fmt.Errorf("chart %s/%s is not a valid chart archive: %w", repoURL, chartName, err)
	}

	registry, repoName, chart, chartVersion, err := chartmirror.ParseChartDestination(dest)
	if err != nil {
		return "", err
	}
	pushed, err := chartmirror.NewPusher(c).PushChart(ctx, registry, repoName, chart, chartVersion, archive)
	if err != nil {
		return "", err
	}
	oclog.Printf("Successfully mirrored helm chart %s/%s:%s -> %s\n", repoName, chart, chartVersion, pushed)
	return pushed, nil
}
