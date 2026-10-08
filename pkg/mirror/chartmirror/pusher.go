// Package chartmirror pushes downloaded Helm chart archives into the target
// registry as OCI artifacts (media type application/vnd.cncf.helm.chart.content.v1.tar+gzip),
// the same layout `helm push` produces, so disconnected clusters can install
// the mirrored charts via `oci://<registry>/...`.
package chartmirror

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// ChartLayerMediaType is the layer media type of a Helm chart archive in an
// OCI artifact (what helm push uses today).
const ChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// chartConfigMediaType is the config blob media type of a Helm chart OCI
// artifact. Its content is the (empty) JSON object, as written by helm push.
const chartConfigMediaType = "application/vnd.cncf.helm.config.v1+json"

// chartRepoPrefix is the repository namespace under the target registry all
// mirrored charts are pushed to: <registry>/charts/<repoName>/<chart>:<version>.
const chartRepoPrefix = "charts"

// ChartDestination returns the target OCI reference a chart archive is
// mirrored to: <registry>/charts/<repoName>/<chartName>:<chartVersion>, the
// layout helm push produces.
func ChartDestination(registry, repoName, chartName, chartVersion string) string {
	return fmt.Sprintf("%s/%s/%s/%s:%s", registry, chartRepoPrefix, repoName, chartName, chartVersion)
}

// ParseChartDestination splits a reference produced by ChartDestination
// back into its registry, repository name, chart name and chart version.
// The charts/ namespace segment is located by name (not position), so the
// registry may contain a port and/or a repository path prefix (e.g.
// "registry.example.com:5000/mirror/charts/bitnami/nginx:15.5.1").
func ParseChartDestination(dest string) (registry, repoName, chartName, chartVersion string, err error) {
	ref := dest
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		chartVersion = ref[i+1:]
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	for idx := 1; idx+2 < len(parts); idx++ {
		if parts[idx] == chartRepoPrefix {
			return strings.Join(parts[:idx], "/"), parts[idx+1], parts[idx+2], chartVersion, nil
		}
	}
	return "", "", "", "", fmt.Errorf("invalid chart destination %q", dest)
}

// Pusher pushes chart archives into the target registry as OCI artifacts.
type Pusher struct {
	client *mirrorclient.MirrorClient
}

// NewPusher returns a Pusher pushing through client (the manager's registry
// client for the MirrorTarget, so auth/insecure handling matches images).
func NewPusher(client *mirrorclient.MirrorClient) *Pusher {
	return &Pusher{client: client}
}

// PushChart pushes the chart archive data (a .tgz, as downloaded from its
// repository) as a Helm OCI artifact to
// <registry>/<chartRepoPrefix>/<repoName>/<chartName>:<chartVersion>, the
// layout helm push produces, and returns the pushed reference. A nil client
// (e.g. from a nil MirrorClient) returns a clear error instead of panicking.
func (p *Pusher) PushChart(ctx context.Context, registry, repoName, chartName, chartVersion string, archive []byte) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("push chart %s: no registry client configured", chartName)
	}
	if chartName == "" || chartVersion == "" {
		return "", fmt.Errorf("push chart: name %q and version %q are required", chartName, chartVersion)
	}

	dest := fmt.Sprintf("%s/%s/%s/%s:%s", registry, chartRepoPrefix, repoName, chartName, chartVersion)
	destRef, err := ref.New(dest)
	if err != nil {
		return "", fmt.Errorf("parse chart destination %s: %w", dest, err)
	}

	layerDesc := descriptor.Descriptor{MediaType: ChartLayerMediaType, Digest: digest.FromBytes(archive), Size: int64(len(archive))}
	layerCtx, layerCancel := context.WithTimeout(ctx, 5*time.Minute)
	_, err = p.client.BlobPut(layerCtx, destRef, layerDesc, bytes.NewReader(archive))
	layerCancel()
	if err != nil {
		return "", fmt.Errorf("push chart layer of %s: %w", dest, err)
	}

	cfgData := []byte("{}")
	cfgDesc := descriptor.Descriptor{MediaType: chartConfigMediaType, Digest: digest.FromBytes(cfgData), Size: int64(len(cfgData))}
	cfgCtx, cfgCancel := context.WithTimeout(ctx, 2*time.Minute)
	_, err = p.client.BlobPut(cfgCtx, destRef, cfgDesc, bytes.NewReader(cfgData))
	cfgCancel()
	if err != nil {
		return "", fmt.Errorf("push chart config of %s: %w", dest, err)
	}

	ociManifest := v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    []descriptor.Descriptor{layerDesc},
	}
	m, err := manifest.New(manifest.WithOrig(ociManifest))
	if err != nil {
		return "", fmt.Errorf("create chart manifest for %s: %w", dest, err)
	}

	mfCtx, mfCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = p.client.ManifestPut(mfCtx, destRef, m)
	mfCancel()
	if err != nil {
		return "", fmt.Errorf("push chart manifest %s: %w", dest, err)
	}
	return dest, nil
}
