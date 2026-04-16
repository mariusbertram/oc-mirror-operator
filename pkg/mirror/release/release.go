package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

const (
	OcpUpdateURL = "https://api.openshift.com/api/upgrades_info/v1/graph"
)

type Graph struct {
	Nodes []Node `json:"nodes"`
}

type Node struct {
	Version string `json:"version"`
	Image   string `json:"payload"`
}

type ReleaseResolver struct {
	client *mirrorclient.MirrorClient
}

func New(client *mirrorclient.MirrorClient) *ReleaseResolver {
	return &ReleaseResolver{client: client}
}

func (r *ReleaseResolver) ResolveRelease(ctx context.Context, channel, version string, arch []string) ([]string, error) {
	u, _ := url.Parse(OcpUpdateURL)
	q := u.Query()
	q.Set("channel", channel)
	if len(arch) > 0 {
		q.Set("arch", arch[0])
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Add("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var graph Graph
	if err := json.Unmarshal(body, &graph); err != nil {
		return nil, err
	}

	for _, node := range graph.Nodes {
		if node.Version == version {
			return []string{node.Image}, nil
		}
	}

	return nil, fmt.Errorf("version %s not found in channel %s", version, channel)
}
