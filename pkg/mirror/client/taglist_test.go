package client

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

func TestTagList(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	c := NewMirrorClient([]string{host}, "", host)
	repo, err := ref.New(host + "/mirror/rhacs/scanner-db:4.11.0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.New(manifest.WithOrig(v1.Manifest{
		Versioned: v1.ManifestSchemaVersion, MediaType: mediatype.OCI1Manifest,
		Config: descriptor.Descriptor{MediaType: mediatype.OCI1Empty, Digest: descriptor.EmptyDigest, Size: int64(len(descriptor.EmptyData))},
		Layers: []descriptor.Descriptor{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"4.11.0", "latest"} {
		if err := c.ManifestPut(t.Context(), repo.SetTag(tag), m); err != nil {
			t.Fatal(err)
		}
	}
	tags, err := c.TagList(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tags)
	if !slices.Equal(tags, []string{"4.11.0", "latest"}) {
		t.Fatalf("TagList() = %v", tags)
	}
}
