// Package helm resolves container image references from Helm charts,
// matching oc-mirror v2's approach: charts are downloaded and their
// Kubernetes templates fully rendered (with default values/capabilities),
// then every rendered manifest is scanned via JSONPath for image fields —
// not a static grep of values.yaml, which would miss the majority of
// real-world charts that template image references dynamically.
package helm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
	"helm.sh/helm/v4/pkg/chart/common"
	commonutil "helm.sh/helm/v4/pkg/chart/common/util"
	helmchart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// defaultImagePaths are the JSONPath expressions scanned in every rendered
// manifest by default, matching oc-mirror v2 exactly. Chart.ImagePaths adds
// further expressions on top of these.
var defaultImagePaths = []string{
	"{.spec.template.spec.initContainers[*].image}",
	"{.spec.template.spec.containers[*].image}",
	"{.spec.initContainers[*].image}",
	"{.spec.containers[*].image}",
}

// ociScheme prefixes chart references stored in an OCI registry, both in
// repository URLs and in index.yaml download URLs (e.g. Bitnami's index
// points at oci://registry-1.docker.io/bitnamicharts/<chart>:<version>).
const ociScheme = "oci://"

// helmChartLayerMediaTypes are the layer media types of a Helm chart archive
// in an OCI artifact: the current one and the one Helm 3.0–3.7 pushed.
var helmChartLayerMediaTypes = map[string]bool{
	"application/vnd.cncf.helm.chart.content.v1.tar+gzip": true,
	"application/tar+gzip":                                true,
}

// maxChartArchiveSize bounds a downloaded chart archive.
const maxChartArchiveSize = 64 * 1024 * 1024

// OCIClient is the part of the registry client needed to pull a chart stored
// as an OCI artifact. *mirrorclient.MirrorClient implements it, so OCI charts
// are pulled with the same credentials and insecure-host handling as images.
type OCIClient interface {
	ManifestGet(ctx context.Context, r ref.Ref) (manifest.Manifest, error)
	BlobGet(ctx context.Context, r ref.Ref, d descriptor.Descriptor) (blob.Reader, error)
}

// Resolver downloads Helm charts from repositories and extracts container
// image references from their rendered manifests.
type Resolver struct {
	httpClient *http.Client
	oci        OCIClient
}

// New returns a Resolver for HTTP(S) chart repositories only; charts stored
// in OCI registries (oci://) fail with a clear error. Use NewWithOCI to
// support those too.
func New() *Resolver {
	return NewWithOCI(nil)
}

// NewWithOCI returns a Resolver that pulls oci:// charts through oci.
func NewWithOCI(oci OCIClient) *Resolver {
	return &Resolver{httpClient: &http.Client{Timeout: 2 * time.Minute}, oci: oci}
}

// ResolveChart downloads the named chart from the repository at repoURL
// (resolving chart.Version via the repository's index.yaml — "" resolves to
// the latest non-prerelease version, matching Helm CLI defaults), renders its
// templates, and returns every distinct container image reference found,
// matching defaultImagePaths plus chart.ImagePaths.
//
// For an OCI repository (repoURL oci://<registry>/<namespace>) there is no
// index.yaml; the chart is pulled from <repoURL>/<name>:<version>, so a
// version is required.
func (r *Resolver) ResolveChart(ctx context.Context, repoURL string, chart mirrorv1alpha1.Chart) ([]string, error) {
	ch, err := r.LoadChart(ctx, repoURL, chart)
	if err != nil {
		return nil, err
	}
	return imagesFromChart(ch, chart.ImagePaths...)
}

// LoadChart downloads the named chart from the repository at repoURL (same
// version resolution as ResolveChart) and returns the loaded chart. Use it
// (instead of ResolveChart) when the chart's metadata — e.g. the version an
// empty chart.Version resolved to, needed to tag the chart when mirroring
// it into the target registry — is needed alongside the rendered images.
func (r *Resolver) LoadChart(ctx context.Context, repoURL string, chart mirrorv1alpha1.Chart) (*helmchart.Chart, error) {
	data, _, err := r.DownloadChartArchive(ctx, repoURL, chart)
	if err != nil {
		return nil, err
	}
	return LoadChartArchive(data)
}

// DownloadChartArchive downloads the named chart's raw .tgz archive (same
// version resolution as ResolveChart) and additionally returns the version
// the reference resolved to — chart.Version when set, or the version picked
// from the repository index for an empty chart.Version (always chart.Version
// for oci:// repositories, where it is required). Callers that mirror the
// chart itself into a registry need the resolved version to tag it.
func (r *Resolver) DownloadChartArchive(ctx context.Context, repoURL string, chart mirrorv1alpha1.Chart) ([]byte, string, error) {
	var chartURL, version string
	if strings.HasPrefix(repoURL, ociScheme) {
		if chart.Version == "" {
			return nil, "", fmt.Errorf("chart %q: a version is required for OCI repository %s (there is no index to pick the latest from)", chart.Name, repoURL)
		}
		chartURL = strings.TrimSuffix(repoURL, "/") + "/" + chart.Name + ":" + chart.Version
		version = chart.Version
	} else {
		cv, err := r.resolveChartVersion(ctx, repoURL, chart.Name, chart.Version)
		if err != nil {
			return nil, "", fmt.Errorf("resolve chart URL: %w", err)
		}
		chartURL = cv.URLs[0]
		if !strings.Contains(chartURL, "://") {
			chartURL = strings.TrimSuffix(repoURL, "/") + "/" + strings.TrimPrefix(chartURL, "/")
		}
		version = cv.Version
	}

	var (
		data []byte
		err  error
	)
	if strings.HasPrefix(chartURL, ociScheme) {
		data, err = r.pullOCIChart(ctx, chartURL)
	} else {
		data, err = r.get(ctx, chartURL)
	}
	if err != nil {
		return nil, "", fmt.Errorf("download chart: %w", err)
	}
	return data, version, nil
}

// LoadChartArchive loads a chart archive (.tgz) from an in-memory buffer.
func LoadChartArchive(data []byte) (*helmchart.Chart, error) {
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive: %w", err)
	}
	return ch, nil
}

// ImagesFromChart renders ch's templates and returns every distinct container
// image reference found at defaultImagePaths plus extraPaths. Exported so the
// manager can reuse an already-downloaded archive for both image extraction
// and chart mirroring without a second download.
func ImagesFromChart(ch *helmchart.Chart, extraPaths ...string) ([]string, error) {
	return imagesFromChart(ch, extraPaths...)
}

// resolveChartVersion fetches the repository index and returns the index
// entry for the requested chart name/version. The entry's URLs[0] is the
// download URL; its Version is the version the reference resolved to (which
// may differ from the requested version when chart.Version was empty).
func (r *Resolver) resolveChartVersion(ctx context.Context, repoURL, name, version string) (*helmrepo.ChartVersion, error) {
	base := strings.TrimSuffix(repoURL, "/")
	data, err := r.get(ctx, base+"/index.yaml")
	if err != nil {
		return nil, fmt.Errorf("fetch repository index: %w", err)
	}

	var index helmrepo.IndexFile
	if err := yaml.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse index.yaml: %w", err)
	}
	index.SortEntries()

	cv, err := index.Get(name, version)
	if err != nil {
		return nil, fmt.Errorf("chart %q (version %q) not found in repository index: %w", name, version, err)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("chart %q has no download URL in repository index", name)
	}
	return cv, nil
}

// pullOCIChart pulls the chart archive layer of the OCI artifact at chartURL
// (oci://<registry>/<repo>:<tag> or @<digest>).
func (r *Resolver) pullOCIChart(ctx context.Context, chartURL string) ([]byte, error) {
	if r.oci == nil {
		return nil, fmt.Errorf("%s is stored in an OCI registry, but no registry client is configured", chartURL)
	}
	cref, err := ref.New(strings.TrimPrefix(chartURL, ociScheme))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", chartURL, err)
	}
	m, err := r.oci.ManifestGet(ctx, cref)
	if err != nil {
		return nil, fmt.Errorf("get manifest of %s: %w", chartURL, err)
	}
	imager, ok := m.(manifest.Imager)
	if !ok {
		return nil, fmt.Errorf("%s is not a single-artifact manifest (media type %s)", chartURL, m.GetDescriptor().MediaType)
	}
	layers, err := imager.GetLayers()
	if err != nil {
		return nil, fmt.Errorf("list layers of %s: %w", chartURL, err)
	}
	for _, layer := range layers {
		if !helmChartLayerMediaTypes[layer.MediaType] {
			continue
		}
		br, err := r.oci.BlobGet(ctx, cref, layer)
		if err != nil {
			return nil, fmt.Errorf("get chart layer of %s: %w", chartURL, err)
		}
		defer func() { _ = br.Close() }()
		data, err := io.ReadAll(io.LimitReader(br, maxChartArchiveSize))
		if err != nil {
			return nil, fmt.Errorf("read chart layer of %s: %w", chartURL, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s has no Helm chart layer", chartURL)
}

func (r *Resolver) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChartArchiveSize))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty response body from %s", url)
	}
	return data, nil
}

// imagesFromChart renders ch's templates and scans every resulting manifest
// document for image references at defaultImagePaths plus extraPaths.
// Returns a sorted, de-duplicated list.
func imagesFromChart(ch *helmchart.Chart, extraPaths ...string) ([]string, error) {
	rendered, err := renderChart(ch)
	if err != nil {
		return nil, fmt.Errorf("render chart %s: %w", ch.Name(), err)
	}

	paths := make([]string, 0, len(defaultImagePaths)+len(extraPaths))
	paths = append(paths, defaultImagePaths...)
	paths = append(paths, extraPaths...)

	seen := make(map[string]struct{})
	var images []string
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		found, err := findImages([]byte(doc), paths...)
		if err != nil {
			return nil, err
		}
		for _, img := range found {
			if img == "" {
				continue
			}
			if _, ok := seen[img]; ok {
				continue
			}
			seen[img] = struct{}{}
			images = append(images, img)
		}
	}
	sort.Strings(images)
	return images, nil
}

// renderChart processes dependencies and renders ch's templates using
// default values and capabilities, returning the concatenated, sorted
// manifest documents (mirroring oc-mirror v2's getHelmTemplates).
func renderChart(ch *helmchart.Chart) (string, error) {
	valueOpts := map[string]interface{}{}
	if err := chartutil.ProcessDependencies(ch, valueOpts); err != nil {
		return "", fmt.Errorf("process dependencies: %w", err)
	}

	caps := common.DefaultCapabilities
	renderVals, err := commonutil.ToRenderValues(ch, valueOpts, common.ReleaseOptions{}, caps)
	if err != nil {
		return "", fmt.Errorf("compose render values: %w", err)
	}

	files, err := engine.Render(ch, renderVals)
	if err != nil {
		return "", fmt.Errorf("render templates: %w", err)
	}
	for k := range files {
		if strings.HasSuffix(k, ".txt") {
			delete(files, k)
		}
	}

	var out strings.Builder
	for _, crd := range ch.CRDObjects() {
		fmt.Fprintf(&out, "---\n# Source: %s\n%s\n", crd.Name, string(crd.File.Data))
	}

	_, manifests, err := releaseutil.SortManifests(files, caps.APIVersions, releaseutil.InstallOrder)
	if err != nil {
		// Best-effort fallback, matching oc-mirror v2: return the raw rendered
		// files even if sorting/parsing some of them failed, so a single
		// malformed template doesn't hide images found in the others.
		for name, content := range files {
			if strings.TrimSpace(content) == "" {
				continue
			}
			fmt.Fprintf(&out, "---\n# Source: %s\n%s\n", name, content)
		}
		return out.String(), nil //nolint:nilerr
	}
	for _, m := range manifests {
		fmt.Fprintf(&out, "---\n# Source: %s\n%s\n", m.Name, m.Content)
	}
	return out.String(), nil
}

// findImages parses a single rendered YAML document and evaluates each
// JSONPath expression against it, returning every matched value.
func findImages(doc []byte, paths ...string) ([]string, error) {
	var data interface{}
	if err := yaml.Unmarshal(doc, &data); err != nil {
		return nil, fmt.Errorf("parse rendered manifest: %w", err)
	}
	if data == nil {
		return nil, nil
	}

	jp := jsonpath.New("")
	jp.AllowMissingKeys(true)

	var images []string
	for _, path := range paths {
		results, err := evalJSONPath(data, jp, path)
		if err != nil {
			return nil, fmt.Errorf("evaluate jsonpath %s: %w", path, err)
		}
		images = append(images, results...)
	}
	return images, nil
}

func evalJSONPath(input interface{}, jp *jsonpath.JSONPath, template string) ([]string, error) {
	if err := jp.Parse(template); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, input); err != nil {
		return nil, err
	}
	return strings.Fields(buf.String()), nil
}
