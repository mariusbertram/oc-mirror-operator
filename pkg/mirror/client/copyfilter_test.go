package client

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/platform"
)

// indexChild is one entry of a test manifest list. platform "" leaves the
// entry without a platform; digest "" seeds a real image and uses its digest.
type indexChild struct {
	platform string
	digest   string
}

// seedIndex seeds an OCI index under src:v1 whose entries point at the
// given children. Children without a digest get a real image seeded under
// repo; children with a digest are referenced only, i.e. missing at the
// source. It returns the index digest and the digests of all children.
func seedIndex(t *testing.T, reg *fakeRegistry, children []indexChild) (string, []string) {
	t.Helper()
	const repo, tag = "src", "v1"
	entries := make([]string, 0, len(children))
	digests := make([]string, 0, len(children))
	for i, c := range children {
		d, size := c.digest, 100
		if d == "" {
			d, _ = seedTestImage(t, reg, repo, fmt.Sprintf("child-%d", i))
			reg.mu.Lock()
			size = len(reg.manifests[repo+":"+d])
			reg.mu.Unlock()
		}
		digests = append(digests, d)
		plat := ""
		if c.platform != "" {
			p, err := platform.Parse(c.platform)
			if err != nil {
				t.Fatalf("parse platform %q: %v", c.platform, err)
			}
			plat = fmt.Sprintf(`,"platform":{"os":%q,"architecture":%q`, p.OS, p.Architecture)
			if p.Variant != "" {
				plat += fmt.Sprintf(`,"variant":%q`, p.Variant)
			}
			plat += "}"
		}
		entries = append(entries, fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":%d%s}`, ociManifestMediaType, d, size, plat))
	}
	body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[%s]}`, ociIndexMediaType, strings.Join(entries, ",")))
	return reg.seedManifest(repo, tag, ociIndexMediaType, body), digests
}

// httpOnlyClient speaks plain HTTP to host without the insecure-host HTTPS
// fallback, so the many expected 404s of these tests fail fast instead of
// each also trying (and timing out on) HTTPS.
func httpOnlyClient(host string) *MirrorClient {
	h := map[string]config.Host{host: destHostConfig(host, config.TLSDisabled, false)}
	mc := &MirrorClient{rc: newRegClient("", h)}
	mc.newBuffered = func() *MirrorClient {
		hb := map[string]config.Host{host: destHostConfig(host, config.TLSDisabled, true)}
		return &MirrorClient{rc: newRegClient("", hb)}
	}
	return mc
}

// missingDigest is a well-formed digest no test registry holds.
func missingDigest(s string) string {
	return sha256Digest([]byte("missing-" + s))
}

var skipMissing = CopyFilter{SkipMissing: true}

func hasManifest(reg *fakeRegistry, repo, tagOrDigest string) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	_, ok := reg.manifests[repo+":"+tagOrDigest]
	return ok
}

func TestCopyImageFiltered_SkipMissingSparseIndexKeepsDigest(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%v", buffered), func(t *testing.T) {
			reg, host := newFakeRegistryServer(t)
			s390x := missingDigest("s390x")
			idx, children := seedIndex(t, reg, []indexChild{
				{platform: "linux/amd64"},
				{platform: "linux/arm64/v8"},
				{platform: "linux/s390x", digest: s390x},
			})
			mc := httpOnlyClient(host)

			// Without the switch the copy fails on the missing child (slow:
			// regclient retries the 404, so this is checked only once).
			if !buffered {
				if _, err := mc.CopyImage(context.Background(), host+"/src@"+idx, host+"/plain:v1"); err == nil {
					t.Fatal("expected a plain copy of an index with a missing child to fail")
				}
			}

			eff, skipped, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, host+"/dst:v1", skipMissing, buffered)
			if err != nil {
				t.Fatalf("CopyImageSkipMissing: %v", err)
			}
			if eff == "" {
				t.Error("expected an effective destination")
			}
			if len(skipped) != 1 || !strings.Contains(skipped[0], "linux/s390x") || !strings.Contains(skipped[0], s390x) {
				t.Errorf("skipped = %v, want the s390x child", skipped)
			}
			// The index is pushed unchanged: same digest as the source.
			if !hasManifest(reg, "dst", idx) {
				t.Errorf("destination does not hold the source index digest %s", idx)
			}
			for _, d := range children[:2] {
				if !hasManifest(reg, "dst", d) {
					t.Errorf("existing child %s was not copied", d)
				}
			}
			if hasManifest(reg, "dst", s390x) {
				t.Error("missing child must not appear at the destination")
			}
		})
	}
}

func TestCopyImageFiltered_SkipMissingNothingMissingIsPlainCopy(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	idx, children := seedIndex(t, reg, []indexChild{
		{platform: "linux/amd64"},
		{platform: "linux/arm64"},
	})
	mc := httpOnlyClient(host)
	_, skipped, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, host+"/dst:v1", skipMissing, false)
	if err != nil {
		t.Fatalf("CopyImageSkipMissing: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
	for _, d := range append(children, idx) {
		if !hasManifest(reg, "dst", d) {
			t.Errorf("manifest %s was not copied", d)
		}
	}
}

func TestCopyImageFiltered_SkipMissingSingleImage(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	d, _ := seedTestImage(t, reg, "src", "v1")
	mc := httpOnlyClient(host)
	_, skipped, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+d, host+"/dst:v1", skipMissing, false)
	if err != nil {
		t.Fatalf("CopyImageSkipMissing: %v", err)
	}
	if len(skipped) != 0 || !hasManifest(reg, "dst", d) {
		t.Errorf("skipped = %v, copied = %v; want a plain copy", skipped, hasManifest(reg, "dst", d))
	}
}

func TestCopyImageFiltered_SkipMissingRefusals(t *testing.T) {
	tests := []struct {
		name     string
		children []indexChild
		wantErr  string
	}{
		{
			name: "all children missing",
			children: []indexChild{
				{platform: "linux/amd64", digest: missingDigest("a")},
				{platform: "linux/arm64", digest: missingDigest("b")},
			},
			wantErr: "no child manifest",
		},
		{
			name: "missing child shares platform with existing one",
			children: []indexChild{
				{platform: "linux/amd64"},
				{platform: "linux/amd64", digest: missingDigest("dup")},
			},
			wantErr: "same platform",
		},
		{
			name: "missing and existing child both without platform",
			children: []indexChild{
				{platform: "linux/amd64"},
				{},
				{digest: missingDigest("attestation")},
			},
			wantErr: "same platform",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, host := newFakeRegistryServer(t)
			idx, _ := seedIndex(t, reg, tt.children)
			mc := httpOnlyClient(host)
			_, _, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, host+"/dst:v1", skipMissing, false)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if hasManifest(reg, "dst", idx) {
				t.Error("index must not be pushed when filtering is refused")
			}
		})
	}
}

func TestCopyImageFiltered_SkipMissingSourceErrors(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	_ = reg
	mc := httpOnlyClient(host)
	if _, _, err := mc.CopyImageFiltered(context.Background(), "bad ref!", host+"/dst:v1", skipMissing, false); err == nil {
		t.Error("expected an error for an unparsable source")
	}
	if _, _, err := mc.CopyImageFiltered(context.Background(), host+"/src:absent", host+"/dst:v1", skipMissing, false); err == nil {
		t.Error("expected an error for a missing source")
	}
}

func TestCopyImageFiltered_SkipMissingDestinationRejectsSparseIndex(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	idx, _ := seedIndex(t, reg, []indexChild{
		{platform: "linux/amd64"},
		{platform: "linux/s390x", digest: missingDigest("s390x")},
	})
	mc := httpOnlyClient(host)
	// An unparsable destination stands in for any copy failure after
	// filtering; the error must name the skipped children.
	_, _, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, "Bad/Dest!", skipMissing, false)
	if err == nil || !strings.Contains(err.Error(), "sparse manifest lists") || !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("err = %v, want it to mention the copied platforms and sparse manifest lists", err)
	}
}

func TestSamePlatform(t *testing.T) {
	p := func(s string) *platform.Platform {
		v, err := platform.Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return &v
	}
	tests := []struct {
		a, b *platform.Platform
		want bool
	}{
		{nil, nil, true},
		{nil, &platform.Platform{}, true},
		{nil, p("linux/amd64"), false},
		{p("linux/amd64"), nil, false},
		{p("linux/amd64"), p("linux/amd64"), true},
		{p("linux/arm64"), p("linux/arm64/v8"), true},
		{p("linux/amd64"), p("linux/s390x"), false},
	}
	for _, tt := range tests {
		if got := samePlatform(tt.a, tt.b); got != tt.want {
			t.Errorf("samePlatform(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
	if platformString(nil) != "" || platformString(p("linux/s390x")) != "linux/s390x" {
		t.Error("platformString mismatch")
	}
}

func TestCopyImageFiltered_Architectures(t *testing.T) {
	s390x := missingDigest("s390x")
	tests := []struct {
		name       string
		children   []indexChild
		filter     CopyFilter
		wantCopied []int // indexes of children expected at the destination
		wantMiss   int
		wantErr    string
	}{
		{
			// The Confluent case without SkipMissing: the missing s390x
			// child is never looked at because it is not selected.
			name: "list leaves out broken architecture",
			children: []indexChild{
				{platform: "linux/amd64"}, {platform: "linux/arm64/v8"}, {platform: "linux/s390x", digest: s390x},
			},
			filter:     CopyFilter{Architectures: []string{"amd64", "arm64"}},
			wantCopied: []int{0, 1},
		},
		{
			name: "single architecture and alias",
			children: []indexChild{
				{platform: "linux/amd64"}, {platform: "linux/arm64"}, {platform: "linux/ppc64le"},
			},
			filter:     CopyFilter{Architectures: []string{"x86_64"}},
			wantCopied: []int{0},
		},
		{
			name: "attestation children are dropped",
			children: []indexChild{
				{platform: "linux/amd64"}, {platform: "unknown/unknown"}, {},
			},
			filter:     CopyFilter{Architectures: []string{"amd64"}},
			wantCopied: []int{0},
		},
		{
			name: "list and skip missing combined",
			children: []indexChild{
				{platform: "linux/amd64"}, {platform: "linux/arm64", digest: missingDigest("arm64")}, {platform: "linux/s390x"},
			},
			filter:     CopyFilter{Architectures: []string{"amd64", "arm64"}, SkipMissing: true},
			wantCopied: []int{0},
			wantMiss:   1,
		},
		{
			name: "list selecting everything is a plain copy",
			children: []indexChild{
				{platform: "linux/amd64"}, {platform: "linux/arm64"},
			},
			filter:     CopyFilter{Architectures: []string{"amd64", "arm64"}},
			wantCopied: []int{0, 1},
		},
		{
			name: "no requested architecture in the list",
			children: []indexChild{
				{platform: "linux/arm64"},
			},
			filter:  CopyFilter{Architectures: []string{"amd64"}},
			wantErr: "no child manifest",
		},
		{
			name: "requested architecture missing without skip",
			children: []indexChild{
				{platform: "linux/amd64", digest: missingDigest("amd64")}, {platform: "linux/arm64"},
			},
			filter:  CopyFilter{Architectures: []string{"amd64"}},
			wantErr: "failed to copy image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, host := newFakeRegistryServer(t)
			idx, children := seedIndex(t, reg, tt.children)
			mc := httpOnlyClient(host)
			_, missing, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, host+"/dst:v1", tt.filter, false)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CopyImageFiltered: %v", err)
			}
			if len(missing) != tt.wantMiss {
				t.Errorf("missing = %v, want %d entries", missing, tt.wantMiss)
			}
			if !hasManifest(reg, "dst", idx) {
				t.Error("source index digest not preserved at the destination")
			}
			want := map[int]bool{}
			for _, i := range tt.wantCopied {
				want[i] = true
			}
			for i, d := range children {
				if got := hasManifest(reg, "dst", d); got != want[i] {
					t.Errorf("child %d (%s) copied = %v, want %v", i, tt.children[i].platform, got, want[i])
				}
			}
		})
	}
}

func TestCopyImageFiltered_ZeroFilterAndSingleImage(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	d, _ := seedTestImage(t, reg, "src", "v1")
	mc := httpOnlyClient(host)
	for i, f := range []CopyFilter{{}, {Architectures: []string{"s390x"}}} {
		dst := fmt.Sprintf("dst%d", i)
		if _, _, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+d, host+"/"+dst+":v1", f, i == 1); err != nil {
			t.Fatalf("filter %+v: %v", f, err)
		}
		if !hasManifest(reg, dst, d) {
			t.Errorf("filter %+v: single-arch image not copied", f)
		}
	}
	if !(CopyFilter{}).IsZero() || (CopyFilter{SkipMissing: true}).IsZero() || (CopyFilter{Architectures: []string{"amd64"}}).IsZero() {
		t.Error("IsZero mismatch")
	}
}

func TestArchSelected(t *testing.T) {
	amd := &platform.Platform{OS: "linux", Architecture: "amd64"}
	arm := &platform.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	if !archSelected(amd, []string{"AMD64"}) || !archSelected(arm, []string{"aarch64"}) {
		t.Error("expected case-insensitive and alias matches")
	}
	if archSelected(nil, []string{"amd64"}) || archSelected(&platform.Platform{OS: "linux"}, []string{"amd64"}) {
		t.Error("children without architecture must not be selected")
	}
	if archSelected(arm, []string{"amd64"}) {
		t.Error("arm64 must not match amd64")
	}
}

// Signatures and attachments of a filtered image must still be mirrored: the
// cosign ".sig" tag and the OCI referrers (signatures, attestations, SBOMs)
// both hang off the manifest list digest, which filtering preserves.
func TestCopyImageFiltered_CopiesSignaturesAndReferrers(t *testing.T) {
	for name, f := range map[string]CopyFilter{
		"skip missing":  {SkipMissing: true},
		"architectures": {Architectures: []string{"amd64"}},
	} {
		t.Run(name, func(t *testing.T) {
			reg, host := newFakeRegistryServer(t)
			idx, _ := seedIndex(t, reg, []indexChild{
				{platform: "linux/amd64"},
				{platform: "linux/s390x", digest: missingDigest("s390x")},
			})

			// Tag-based cosign signature of the manifest list.
			sigTag := "sha256-" + strings.TrimPrefix(idx, "sha256:") + ".sig"
			seedTestImage(t, reg, "src", sigTag)

			// OCI 1.1 referrer (e.g. an attestation) of the manifest list,
			// listed via the referrers fallback tag.
			configDigest := reg.seedBlob([]byte("{}"))
			refBody := []byte(fmt.Sprintf(
				`{"schemaVersion":2,"mediaType":%q,"artifactType":"application/vnd.dsse.envelope.v1+json",`+
					`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},`+
					`"subject":{"mediaType":%q,"digest":%q,"size":100}}`,
				ociManifestMediaType, configDigest, ociIndexMediaType, idx))
			refDigest := reg.seedManifest("src", "sha256:2222222222222222222222222222222222222222222222222222222222222222", ociManifestMediaType, refBody)
			listBody := []byte(fmt.Sprintf(
				`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,`+
					`"artifactType":"application/vnd.dsse.envelope.v1+json"}]}`,
				ociIndexMediaType, ociManifestMediaType, refDigest, len(refBody)))
			reg.seedManifest("src", referrersFallbackTag(idx), ociIndexMediaType, listBody)

			mc := httpOnlyClient(host)
			if _, _, err := mc.CopyImageFiltered(context.Background(), host+"/src@"+idx, host+"/dst:v1", f, false); err != nil {
				t.Fatalf("CopyImageFiltered: %v", err)
			}
			if !hasManifest(reg, "dst", idx) {
				t.Fatal("manifest list digest not preserved")
			}
			if !hasManifest(reg, "dst", sigTag) {
				t.Error("cosign signature tag was not copied")
			}
			if !hasManifest(reg, "dst", refDigest) {
				t.Error("referrer (attestation) was not copied")
			}
		})
	}
}
