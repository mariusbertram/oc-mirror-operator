package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/regclient/regclient/types/ref"
)

func TestCopyImage_CopiesCosignTags(t *testing.T) {
	tests := []struct {
		name     string
		byDigest bool
		present  []string // cosign suffixes seeded at the source
	}{
		{name: "all suffixes, source by digest", byDigest: true, present: []string{".sig", ".att", ".sbom"}},
		{name: "all suffixes, source by tag", byDigest: false, present: []string{".sig", ".att", ".sbom"}},
		{name: "only attestation", byDigest: true, present: []string{".att"}},
		{name: "none", byDigest: false, present: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, host := newFakeRegistryServer(t)
			digest, _ := seedTestImage(t, reg, "src", "v1")
			base := "sha256-" + strings.TrimPrefix(digest, "sha256:")
			for _, s := range tt.present {
				seedTestImage(t, reg, "src", base+s)
			}
			src := host + "/src:v1"
			if tt.byDigest {
				src = host + "/src@" + digest
			}
			mc := httpOnlyClient(host)
			if _, err := mc.CopyImage(context.Background(), src, host+"/dst:v1"); err != nil {
				t.Fatalf("CopyImage: %v", err)
			}
			want := map[string]bool{}
			for _, s := range tt.present {
				want[s] = true
			}
			for _, s := range cosignTagSuffixes {
				if got := hasManifest(reg, "dst", base+s); got != want[s] {
					t.Errorf("%s copied = %v, want %v", s, got, want[s])
				}
			}
		})
	}
}

// A failure to resolve the source digest or to check a tag only skips the
// best-effort cosign copy.
func TestCopyCosignTagsBestEffort_Errors(t *testing.T) {
	_, host := newFakeRegistryServer(t)
	dead := reservedDeadAddr(t)
	mc := httpOnlyClient(host)
	src, err := ref.New(dead + "/src:v1")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := ref.New(host + "/dst:v1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	mc.copyCosignTagsBestEffort(ctx, mc.rc, src, dst) // unresolvable tag source
	src = src.SetDigest("sha256:" + strings.Repeat("a", 64))
	mc.copyCosignTagsBestEffort(ctx, mc.rc, src, dst) // tag checks fail with non-404 errors
}
