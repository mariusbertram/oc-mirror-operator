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

	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	helmrepo "helm.sh/helm/v3/pkg/repo"
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

// Resolver downloads Helm charts from repositories and extracts container
// image references from their rendered manifests.
type Resolver struct {
	httpClient *http.Client
}

func New() *Resolver {
	return &Resolver{httpClient: &http.Client{Timeout: 2 * time.Minute}}
}

// ResolveChart downloads the named chart from the repository at repoURL
// (resolving chart.Version via the repository's index.yaml — "" resolves to
// the latest non-prerelease version, matching Helm CLI defaults), renders its
// templates, and returns every distinct container image reference found,
// matching defaultImagePaths plus chart.ImagePaths.
func (r *Resolver) ResolveChart(ctx context.Context, repoURL string, chart mirrorv1alpha1.Chart) ([]string, error) {
	chartURL, err := r.resolveChartURL(ctx, repoURL, chart.Name, chart.Version)
	if err != nil {
		return nil, fmt.Errorf("resolve chart URL: %w", err)
	}

	ch, err := r.downloadChart(ctx, chartURL)
	if err != nil {
		return nil, fmt.Errorf("download chart: %w", err)
	}

	return imagesFromChart(ch, chart.ImagePaths...)
}

// resolveChartURL fetches the repository index and returns the absolute
// download URL for the requested chart name/version.
func (r *Resolver) resolveChartURL(ctx context.Context, repoURL, name, version string) (string, error) {
	base := strings.TrimSuffix(repoURL, "/")
	data, err := r.get(ctx, base+"/index.yaml")
	if err != nil {
		return "", fmt.Errorf("fetch repository index: %w", err)
	}

	var index helmrepo.IndexFile
	if err := yaml.Unmarshal(data, &index); err != nil {
		return "", fmt.Errorf("parse index.yaml: %w", err)
	}
	index.SortEntries()

	cv, err := index.Get(name, version)
	if err != nil {
		return "", fmt.Errorf("chart %q (version %q) not found in repository index: %w", name, version, err)
	}
	if len(cv.URLs) == 0 {
		return "", fmt.Errorf("chart %q has no download URL in repository index", name)
	}

	chartURL := cv.URLs[0]
	if !strings.Contains(chartURL, "://") {
		chartURL = base + "/" + strings.TrimPrefix(chartURL, "/")
	}
	return chartURL, nil
}

// downloadChart fetches and loads a chart archive (.tgz) directly from an
// in-memory buffer — chart archives are small enough (typically KB to a few
// MB) that no temp file is needed.
func (r *Resolver) downloadChart(ctx context.Context, chartURL string) (*helmchart.Chart, error) {
	data, err := r.get(ctx, chartURL)
	if err != nil {
		return nil, fmt.Errorf("fetch chart archive: %w", err)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive: %w", err)
	}
	return ch, nil
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
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

	caps := chartutil.DefaultCapabilities
	renderVals, err := chartutil.ToRenderValues(ch, valueOpts, chartutil.ReleaseOptions{}, caps)
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
