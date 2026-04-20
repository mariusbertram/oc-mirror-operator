package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

const (
	OcpUpdateURL = "https://api.openshift.com/api/upgrades_info/v1/graph"
)

type Graph struct {
	Nodes []Node  `json:"nodes"`
	Edges [][]int `json:"edges"` // each edge is [from_index, to_index]
}

type Node struct {
	Version string `json:"version"`
	Image   string `json:"payload"`
}

type ReleaseResolver struct {
	client     *mirrorclient.MirrorClient
	httpClient *http.Client
}

func New(client *mirrorclient.MirrorClient) *ReleaseResolver {
	return &ReleaseResolver{
		client: client,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchGraph retrieves the raw Cincinnati upgrade graph for the given channel and arch.
// It is exported so callers (e.g., integration tests) can inspect nodes and edges directly.
func (r *ReleaseResolver) FetchGraph(ctx context.Context, channel string, arch []string) (*Graph, error) {
	u, err := url.Parse(OcpUpdateURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse update URL: %w", err)
	}

	q := u.Query()
	q.Set("channel", channel)
	if len(arch) > 0 {
		q.Set("arch", arch[0])
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Add("Accept", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch release graph: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, u.String())
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var graph Graph
	if err := json.Unmarshal(body, &graph); err != nil {
		return nil, fmt.Errorf("failed to unmarshal release graph: %w", err)
	}
	return &graph, nil
}

func (r *ReleaseResolver) ResolveRelease(ctx context.Context, channel, minVersion, maxVersion string, arch []string, full, shortestPath bool) ([]string, error) {
	graph, err := r.FetchGraph(ctx, channel, arch)
	if err != nil {
		return nil, err
	}

	// full: return all nodes in the channel
	if full {
		var images []string
		for _, node := range graph.Nodes {
			images = append(images, node.Image)
		}
		return images, nil
	}

	// only maxVersion: return just that node (original behavior)
	if minVersion == "" && maxVersion != "" {
		for _, node := range graph.Nodes {
			if node.Version == maxVersion {
				return []string{node.Image}, nil
			}
		}
		return nil, fmt.Errorf("version %s not found in channel %s", maxVersion, channel)
	}

	// both minVersion and maxVersion set
	if minVersion != "" && maxVersion != "" {
		if shortestPath {
			return r.shortestPath(graph, minVersion, maxVersion)
		}
		minSV, err := semver.ParseTolerant(minVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to parse minVersion %s: %w", minVersion, err)
		}
		maxSV, err := semver.ParseTolerant(maxVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to parse maxVersion %s: %w", maxVersion, err)
		}
		var images []string
		for _, node := range graph.Nodes {
			sv, parseErr := semver.ParseTolerant(node.Version)
			if parseErr != nil {
				continue
			}
			if sv.GTE(minSV) && sv.LTE(maxSV) {
				images = append(images, node.Image)
			}
		}
		return images, nil
	}

	// only minVersion set
	if minVersion != "" {
		minSV, err := semver.ParseTolerant(minVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to parse minVersion %s: %w", minVersion, err)
		}
		var images []string
		for _, node := range graph.Nodes {
			sv, parseErr := semver.ParseTolerant(node.Version)
			if parseErr != nil {
				continue
			}
			if sv.GTE(minSV) {
				images = append(images, node.Image)
			}
		}
		return images, nil
	}

	// no constraints: return only the latest version in the channel
	latest := latestNode(graph)
	if latest == nil {
		return nil, fmt.Errorf("no nodes found in channel %s", channel)
	}
	fmt.Printf("No version constraint specified for channel %s, defaulting to latest: %s\n", channel, latest.Version)
	return []string{latest.Image}, nil
}

// ResolveLatestVersion returns the highest semver version available in the given channel.
// It is used by the collector to determine the destination path when no version is pinned.
func (r *ReleaseResolver) ResolveLatestVersion(ctx context.Context, channel string, arch []string) (string, error) {
	graph, err := r.FetchGraph(ctx, channel, arch)
	if err != nil {
		return "", err
	}
	n := latestNode(graph)
	if n == nil {
		return "", fmt.Errorf("no nodes found in channel %s", channel)
	}
	return n.Version, nil
}

// latestNode returns the node with the highest semver in the graph.
func latestNode(graph *Graph) *Node {
	var best *Node
	for i := range graph.Nodes {
		node := &graph.Nodes[i]
		if best == nil {
			best = node
			continue
		}
		sv, err := semver.ParseTolerant(node.Version)
		if err != nil {
			continue
		}
		bestSV, err := semver.ParseTolerant(best.Version)
		if err != nil {
			best = node
			continue
		}
		if sv.GT(bestSV) {
			best = node
		}
	}
	return best
}

// imageStream is a minimal representation of an OpenShift ImageStream used to
// parse the release-manifests/image-references file embedded in a release payload.
type imageStream struct {
	Spec struct {
		Tags []struct {
			Name string `json:"name"`
			From struct {
				Name string `json:"name"`
			} `json:"from"`
		} `json:"tags"`
	} `json:"spec"`
}

// ExtractComponentImages pulls the release payload image, locates the
// release-manifests/image-references layer, and returns all ~190 component
// image references contained in it.
//
// If the payload is a multi-arch manifest list, the platform-specific manifest
// for the requested arch is resolved first. The caller must provide a non-nil
// MirrorClient when constructing the ReleaseResolver via New().
func (r *ReleaseResolver) ExtractComponentImages(ctx context.Context, payloadImage, arch string) ([]string, error) {
	if r.client == nil {
		return nil, fmt.Errorf("ExtractComponentImages requires a non-nil MirrorClient")
	}

	imgRef, err := ref.New(payloadImage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse payload image reference %q: %w", payloadImage, err)
	}

	m, err := r.client.ManifestGet(ctx, imgRef)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest for %s: %w", payloadImage, err)
	}

	// If the manifest is a list (multi-arch), resolve the requested platform.
	if m.IsList() {
		p, parseErr := platform.Parse(fmt.Sprintf("linux/%s", arch))
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse platform linux/%s: %w", arch, parseErr)
		}
		desc, descErr := manifest.GetPlatformDesc(m, &p)
		if descErr != nil {
			return nil, fmt.Errorf("no manifest found for linux/%s in %s: %w", arch, payloadImage, descErr)
		}
		imgRef.Digest = desc.Digest.String()
		imgRef.Tag = ""
		m, err = r.client.ManifestGet(ctx, imgRef)
		if err != nil {
			return nil, fmt.Errorf("failed to get platform manifest for %s: %w", payloadImage, err)
		}
	}

	layers, err := m.GetLayers()
	if err != nil {
		return nil, fmt.Errorf("failed to get layers from manifest: %w", err)
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("release payload %s has no layers", payloadImage)
	}

	// image-references is placed in the release-manifests overlay layer.
	// Scan layers from last to first — it is typically the last or second-to-last.
	for i := len(layers) - 1; i >= 0; i-- {
		blobRdr, blobErr := r.client.BlobGet(ctx, imgRef, layers[i])
		if blobErr != nil {
			continue
		}
		images, found, scanErr := scanLayerForImageReferences(blobRdr)
		_ = blobRdr.Close()
		if scanErr != nil || !found {
			continue
		}
		return images, nil
	}

	return nil, fmt.Errorf("release-manifests/image-references not found in any layer of %s", payloadImage)
}

// scanLayerForImageReferences reads a gzipped tar layer and looks for
// release-manifests/image-references. Returns (images, found, error).
func scanLayerForImageReferences(rdr io.Reader) ([]string, bool, error) {
	gz, err := gzip.NewReader(rdr)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, false, fmt.Errorf("tar read error: %w", nextErr)
		}

		if !strings.HasSuffix(hdr.Name, "image-references") {
			continue
		}

		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			return nil, false, fmt.Errorf("failed to read image-references: %w", readErr)
		}

		var is imageStream
		if jsonErr := json.Unmarshal(data, &is); jsonErr != nil {
			return nil, false, fmt.Errorf("failed to parse image-references: %w", jsonErr)
		}

		images := make([]string, 0, len(is.Spec.Tags))
		for _, tag := range is.Spec.Tags {
			if tag.From.Name != "" {
				images = append(images, tag.From.Name)
			}
		}
		return images, true, nil
	}
	return nil, false, nil
}

// shortestPath uses BFS over the Cincinnati graph edges to find the shortest
func (r *ReleaseResolver) shortestPath(graph *Graph, fromVersion, toVersion string) ([]string, error) {
	versionIdx := make(map[string]int, len(graph.Nodes))
	for i, node := range graph.Nodes {
		versionIdx[node.Version] = i
	}

	fromIdx, ok := versionIdx[fromVersion]
	if !ok {
		return nil, fmt.Errorf("version %s not found in graph", fromVersion)
	}
	toIdx, ok := versionIdx[toVersion]
	if !ok {
		return nil, fmt.Errorf("version %s not found in graph", toVersion)
	}

	adj := make(map[int][]int, len(graph.Nodes))
	for _, edge := range graph.Edges {
		if len(edge) != 2 {
			continue
		}
		adj[edge[0]] = append(adj[edge[0]], edge[1])
	}

	prev := make([]int, len(graph.Nodes))
	for i := range prev {
		prev[i] = -1
	}
	visited := make([]bool, len(graph.Nodes))
	visited[fromIdx] = true
	queue := []int{fromIdx}
	found := false

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == toIdx {
			found = true
			break
		}
		for _, next := range adj[curr] {
			if !visited[next] {
				visited[next] = true
				prev[next] = curr
				queue = append(queue, next)
			}
		}
	}

	if !found {
		return nil, fmt.Errorf("no upgrade path found from %s to %s", fromVersion, toVersion)
	}

	var path []int
	for curr := toIdx; curr != fromIdx; curr = prev[curr] {
		path = append([]int{curr}, path...)
	}
	path = append([]int{fromIdx}, path...)

	images := make([]string, 0, len(path))
	for _, idx := range path {
		images = append(images, graph.Nodes[idx].Image)
	}
	return images, nil
}
