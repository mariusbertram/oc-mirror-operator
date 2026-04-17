package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/blang/semver/v4"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
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

func (r *ReleaseResolver) ResolveRelease(ctx context.Context, channel, minVersion, maxVersion string, arch []string, full, shortestPath bool) ([]string, error) {
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
			return r.shortestPath(&graph, minVersion, maxVersion)
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

	return nil, fmt.Errorf("no version constraints specified for channel %s", channel)
}

// shortestPath uses BFS over the Cincinnati graph edges to find the shortest
// upgrade path between two versions and returns their images.
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
