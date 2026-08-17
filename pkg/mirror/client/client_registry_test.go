package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	regscheme "github.com/regclient/regclient/scheme/reg"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	v1 "github.com/regclient/regclient/types/oci/v1"
)

const (
	registryPingPath     = "/v2/"
	manifestsSegment     = "/manifests/"
	blobsSegment         = "/blobs/"
	uploadsSegment       = "/blobs/uploads/"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ociConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	ociLayerMediaType    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// fakeRegistry is a minimal, stateful, push+pull-capable OCI registry used to
// exercise MirrorClient's manifest/blob GET, HEAD, and PUT paths over real
// HTTP, matching regclient v0.11.5's wire protocol (see
// github.com/regclient/regclient/scheme/reg/{blob,manifest}.go). Blob storage
// is content-addressed (keyed by digest only, not per-repository), matching
// how a real registry would recognise the same blob mounted/pushed under
// different repositories.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string][]byte // key: "repo:tagOrDigest"
	mediaType map[string]string // key: "repo:tagOrDigest"
	blobs     map[string][]byte // key: digest
}

func newFakeRegistryHandler() *fakeRegistry {
	return &fakeRegistry{
		manifests: map[string][]byte{},
		mediaType: map[string]string{},
		blobs:     map[string][]byte{},
	}
}

// newFakeRegistryServer starts a plain-HTTP httptest server backed by a
// fresh fakeRegistry, returning the registry and its host:port.
func newFakeRegistryServer(t *testing.T) (*fakeRegistry, string) {
	t.Helper()
	reg := newFakeRegistryHandler()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	return reg, strings.TrimPrefix(srv.URL, "http://")
}

// reservedDeadAddr reserves an ephemeral TCP port and immediately releases
// it, yielding an address nothing is listening on. Connections to it are
// refused instantly (unlike a live server reached with the wrong protocol,
// which can incur slow, retried timeouts), making it a fast, deterministic
// stand-in for "this leg of the request fails".
func reservedDeadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a dead address: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return addr
}

// splitClient builds a MirrorClient whose primary (rc) and fallback
// (rcFallback) regclients share one symbolic registry name but are each
// pinned, via config.Host.Hostname, to a specific network address. This lets
// tests deterministically choose which of rc/rcFallback actually succeeds
// (by pointing it at a live fakeRegistry, and the other at a dead address)
// without depending on slow, real-world TLS/plaintext protocol-mismatch
// timeouts to force a failure.
func splitClient(symbolicHost, primaryAddr, fallbackAddr string) *MirrorClient {
	newHostClient := func(addr string) *regclient.RegClient {
		return regclient.New(
			regclient.WithDockerCreds(),
			// A connection-refused failure is otherwise retried (with
			// exponential backoff) by regclient's default retry limit of 5,
			// costing ~3s per op; 1 is enough to still prove the
			// primary/fallback branch while keeping the suite fast.
			regclient.WithRegOpts(regscheme.WithRetryLimit(1)),
			regclient.WithConfigHost(config.Host{
				Name:     symbolicHost,
				Hostname: addr,
				TLS:      config.TLSDisabled,
				BlobMax:  -1,
			}),
		)
	}
	return &MirrorClient{
		rc:         newHostClient(primaryAddr),
		rcFallback: newHostClient(fallbackAddr),
	}
}

// symbolicRegistryHost is the registry name used to build refs in
// runRegistryOps; splitClient maps it to real addresses per-client.
const symbolicRegistryHost = "split-fallback.test"

func sha256Digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func (f *fakeRegistry) seedManifest(repo, tagOrDigest, mediaType string, body []byte) string {
	digest := sha256Digest(body)
	f.mu.Lock()
	for _, key := range []string{repo + ":" + tagOrDigest, repo + ":" + digest} {
		f.manifests[key] = body
		f.mediaType[key] = mediaType
	}
	f.mu.Unlock()
	return digest
}

// seedManifestTagOnly seeds a manifest reachable by tag but not by digest,
// simulating a manifest that vanishes between a tag→digest HEAD resolution
// and the subsequent digest-addressed DELETE.
func (f *fakeRegistry) seedManifestTagOnly(repo, tag, mediaType string, body []byte) {
	f.mu.Lock()
	f.manifests[repo+":"+tag] = body
	f.mediaType[repo+":"+tag] = mediaType
	f.mu.Unlock()
}

func (f *fakeRegistry) seedBlob(data []byte) string {
	digest := sha256Digest(data)
	f.mu.Lock()
	f.blobs[digest] = data
	f.mu.Unlock()
	return digest
}

// seedTestImage seeds a complete, valid single-layer OCI image (config blob,
// layer blob, and manifest) under repo:tagOrDigest, returning the manifest
// digest and the descriptor for its layer blob.
func seedTestImage(t *testing.T, reg *fakeRegistry, repo, tagOrDigest string) (manifestDigest string, layerDesc descriptor.Descriptor) {
	t.Helper()

	layerBytes := []byte("layer-content-for-" + repo + "-" + tagOrDigest)
	layerDigestStr := reg.seedBlob(layerBytes)

	configJSON := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["%s"]}}`, layerDigestStr))
	configDigestStr := reg.seedBlob(configJSON)

	manifestJSON := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		ociManifestMediaType, ociConfigMediaType, configDigestStr, len(configJSON),
		ociLayerMediaType, layerDigestStr, len(layerBytes),
	))
	manifestDigest = reg.seedManifest(repo, tagOrDigest, ociManifestMediaType, manifestJSON)

	layerDesc = descriptor.Descriptor{MediaType: ociLayerMediaType, Digest: godigest.Digest(layerDigestStr), Size: int64(len(layerBytes))}
	return manifestDigest, layerDesc
}

func splitAtSegment(path, segment string) (before, after string, ok bool) {
	idx := strings.Index(path, segment)
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimPrefix(path[:idx], "/v2/"), path[idx+len(segment):], true
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == registryPingPath || path == "/v2":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && strings.HasSuffix(path, uploadsSegment):
		w.Header().Set("Location", path+"upload-1")
		w.Header().Set("Docker-Upload-UUID", "upload-1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(path, uploadsSegment):
		f.putBlob(w, r)
	case r.Method == http.MethodPut && strings.Contains(path, manifestsSegment):
		f.putManifest(w, r, path)
	case r.Method == http.MethodDelete && strings.Contains(path, manifestsSegment):
		f.deleteManifest(w, r, path)
	case r.Method == http.MethodHead && strings.Contains(path, manifestsSegment):
		f.headManifest(w, r, path)
	case strings.Contains(path, manifestsSegment):
		f.getManifest(w, r, path)
	case r.Method == http.MethodHead && strings.Contains(path, blobsSegment):
		f.headBlob(w, r, path)
	case strings.Contains(path, blobsSegment):
		f.getBlob(w, r, path)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) putBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.URL.Query().Get("digest")
	body, _ := io.ReadAll(r.Body)
	if digest == "" {
		digest = sha256Digest(body)
	}
	f.mu.Lock()
	f.blobs[digest] = body
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) putManifest(w http.ResponseWriter, r *http.Request, path string) {
	repo, tagOrDigest, ok := splitAtSegment(path, manifestsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	digest := sha256Digest(body)
	mt := r.Header.Get("Content-Type")
	f.mu.Lock()
	for _, key := range []string{repo + ":" + tagOrDigest, repo + ":" + digest} {
		f.manifests[key] = body
		f.mediaType[key] = mt
	}
	f.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) deleteManifest(w http.ResponseWriter, r *http.Request, path string) {
	repo, tagOrDigest, ok := splitAtSegment(path, manifestsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	key := repo + ":" + tagOrDigest
	f.mu.Lock()
	_, found := f.manifests[key]
	delete(f.manifests, key)
	delete(f.mediaType, key)
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeRegistry) headManifest(w http.ResponseWriter, r *http.Request, path string) {
	repo, tagOrDigest, ok := splitAtSegment(path, manifestsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	body, found := f.manifests[repo+":"+tagOrDigest]
	mt := f.mediaType[repo+":"+tagOrDigest]
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Docker-Content-Digest", sha256Digest(body))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) getManifest(w http.ResponseWriter, r *http.Request, path string) {
	repo, tagOrDigest, ok := splitAtSegment(path, manifestsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	body, found := f.manifests[repo+":"+tagOrDigest]
	mt := f.mediaType[repo+":"+tagOrDigest]
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Docker-Content-Digest", sha256Digest(body))
	_, _ = w.Write(body)
}

func (f *fakeRegistry) headBlob(w http.ResponseWriter, r *http.Request, path string) {
	_, digest, ok := splitAtSegment(path, blobsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	body, found := f.blobs[digest]
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) getBlob(w http.ResponseWriter, r *http.Request, path string) {
	_, digest, ok := splitAtSegment(path, blobsSegment)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	body, found := f.blobs[digest]
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

// registryOp exercises one MirrorClient method against a freshly seeded
// fakeRegistry. The same op list is run twice (see TestClientOps_*): once
// against a plain-HTTP server (primary succeeds directly) and once against an
// HTTPS server (primary fails, HTTPS-skip-verify fallback succeeds) — proving
// both the "primary succeeds" and "primary fails, fallback succeeds" branches
// of every rc/rcFallback wrapper method share the same underlying behavior.
type registryOp struct {
	name string
	run  func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string)
}

var registryOps = []registryOp{
	{
		name: "ManifestHead",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			digest, _ := seedTestImage(t, reg, "repo-manifest-head", "v1")
			m, err := mc.ManifestHead(context.Background(), mustParseRef(host+"/repo-manifest-head:v1"))
			if err != nil {
				t.Fatalf("ManifestHead: %v", err)
			}
			if got := m.GetDescriptor().Digest.String(); got != digest {
				t.Errorf("expected digest %s, got %s", digest, got)
			}
		},
	},
	{
		name: "ManifestGet",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			digest, _ := seedTestImage(t, reg, "repo-manifest-get", "v1")
			m, err := mc.ManifestGet(context.Background(), mustParseRef(host+"/repo-manifest-get:v1"))
			if err != nil {
				t.Fatalf("ManifestGet: %v", err)
			}
			if got := m.GetDescriptor().Digest.String(); got != digest {
				t.Errorf("expected digest %s, got %s", digest, got)
			}
		},
	},
	{
		name: "BlobGet",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			_, layerDesc := seedTestImage(t, reg, "repo-blob-get", "v1")
			br, err := mc.BlobGet(context.Background(), mustParseRef(host+"/repo-blob-get:v1"), layerDesc)
			if err != nil {
				t.Fatalf("BlobGet: %v", err)
			}
			defer func() { _ = br.Close() }()
			data, err := io.ReadAll(br)
			if err != nil {
				t.Fatalf("read blob: %v", err)
			}
			if int64(len(data)) != layerDesc.Size {
				t.Errorf("expected %d bytes, got %d", layerDesc.Size, len(data))
			}
		},
	},
	{
		name: "BlobExists_True",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			_, layerDesc := seedTestImage(t, reg, "repo-blob-exists", "v1")
			if !mc.BlobExists(context.Background(), mustParseRef(host+"/repo-blob-exists:v1"), layerDesc) {
				t.Error("expected BlobExists to return true for a seeded blob")
			}
		},
	},
	{
		name: "BlobExists_False",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			missing := descriptor.Descriptor{Digest: godigest.Digest(sha256Digest([]byte("never-seeded"))), Size: 4}
			if mc.BlobExists(context.Background(), mustParseRef(host+"/repo-blob-exists-missing:v1"), missing) {
				t.Error("expected BlobExists to return false for a blob that was never seeded")
			}
		},
	},
	{
		name: "ImageConfig",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			seedTestImage(t, reg, "repo-image-config", "v1")
			cfg, err := mc.ImageConfig(context.Background(), mustParseRef(host+"/repo-image-config:v1"))
			if err != nil {
				t.Fatalf("ImageConfig: %v", err)
			}
			if cfg == nil {
				t.Error("expected non-nil image config")
			}
		},
	},
	{
		name: "BlobPut",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			data := []byte("blob put content for " + host)
			d := descriptor.Descriptor{Digest: godigest.FromBytes(data), Size: int64(len(data))}
			_, err := mc.BlobPut(context.Background(), mustParseRef(host+"/repo-blob-put:v1"), d, bytes.NewReader(data))
			if err != nil {
				t.Fatalf("BlobPut: %v", err)
			}
			reg.mu.Lock()
			stored, ok := reg.blobs[d.Digest.String()]
			reg.mu.Unlock()
			if !ok || !bytes.Equal(stored, data) {
				t.Error("expected the pushed blob to be stored on the registry")
			}
		},
	},
	{
		name: "ManifestPut",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			configJSON := []byte(`{}`)
			configDigest := reg.seedBlob(configJSON)
			m, err := manifest.New(manifest.WithOrig(v1.Manifest{
				Versioned: v1.ManifestSchemaVersion,
				MediaType: ociManifestMediaType,
				Config: descriptor.Descriptor{
					MediaType: ociConfigMediaType,
					Digest:    godigest.Digest(configDigest),
					Size:      int64(len(configJSON)),
				},
			}))
			if err != nil {
				t.Fatalf("build manifest: %v", err)
			}
			if err := mc.ManifestPut(context.Background(), mustParseRef(host+"/repo-manifest-put:v1"), m); err != nil {
				t.Fatalf("ManifestPut: %v", err)
			}
			reg.mu.Lock()
			_, ok := reg.manifests["repo-manifest-put:v1"]
			reg.mu.Unlock()
			if !ok {
				t.Error("expected the pushed manifest to be stored on the registry")
			}
		},
	},
	{
		name: "BlobCopy",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			_, layerDesc := seedTestImage(t, reg, "repo-blob-copy-src", "v1")
			src := mustParseRef(host + "/repo-blob-copy-src:v1")
			dst := mustParseRef(host + "/repo-blob-copy-dst:v1")
			if err := mc.BlobCopy(context.Background(), src, dst, layerDesc); err != nil {
				t.Fatalf("BlobCopy: %v", err)
			}
		},
	},
	{
		name: "GetDigest",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			digest, _ := seedTestImage(t, reg, "repo-get-digest", "v1")
			got, err := mc.GetDigest(context.Background(), host+"/repo-get-digest:v1")
			if err != nil {
				t.Fatalf("GetDigest: %v", err)
			}
			if got != digest {
				t.Errorf("expected digest %s, got %s", digest, got)
			}
		},
	},
	{
		name: "CheckExist_True",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			seedTestImage(t, reg, "repo-check-exist-true", "v1")
			exists, err := mc.CheckExist(context.Background(), host+"/repo-check-exist-true:v1")
			if err != nil {
				t.Fatalf("CheckExist: %v", err)
			}
			if !exists {
				t.Error("expected exists = true")
			}
		},
	},
	{
		name: "CheckExist_False",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			exists, err := mc.CheckExist(context.Background(), host+"/repo-check-exist-missing:v1")
			if err != nil {
				t.Fatalf("CheckExist: %v", err)
			}
			if exists {
				t.Error("expected exists = false")
			}
		},
	},
	{
		name: "DeleteManifest_ByTag",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			seedTestImage(t, reg, "repo-delete-by-tag", "v1")
			if err := mc.DeleteManifest(context.Background(), host+"/repo-delete-by-tag:v1"); err != nil {
				t.Fatalf("DeleteManifest: %v", err)
			}
		},
	},
	{
		name: "DeleteManifest_AlreadyGone",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			if err := mc.DeleteManifest(context.Background(), host+"/repo-delete-missing:v1"); err != nil {
				t.Fatalf("expected nil error for an already-gone manifest, got: %v", err)
			}
		},
	},
	{
		name: "DeleteManifest_VanishesBetweenHeadAndDelete",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			manifestJSON := []byte(fmt.Sprintf(
				`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`,
				ociManifestMediaType, ociConfigMediaType,
			))
			reg.seedManifestTagOnly("repo-delete-race", "v1", ociManifestMediaType, manifestJSON)
			if err := mc.DeleteManifest(context.Background(), host+"/repo-delete-race:v1"); err != nil {
				t.Fatalf("expected nil error when the manifest is already gone by the time delete is attempted, got: %v", err)
			}
		},
	},
	{
		name: "DownloadToOCILayout",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			seedTestImage(t, reg, "repo-download", "v1")
			if err := mc.DownloadToOCILayout(context.Background(), host+"/repo-download:v1", t.TempDir()); err != nil {
				t.Fatalf("DownloadToOCILayout: %v", err)
			}
		},
	},
	{
		name: "DownloadToOCILayout_WithPlatforms",
		run: func(t *testing.T, mc *MirrorClient, reg *fakeRegistry, host string) {
			seedTestImage(t, reg, "repo-download-platform", "v1")
			err := mc.DownloadToOCILayout(context.Background(), host+"/repo-download-platform:v1", t.TempDir(), "linux/amd64")
			if err != nil {
				t.Fatalf("DownloadToOCILayout: %v", err)
			}
		},
	},
}

func runRegistryOps(t *testing.T, primarySucceeds bool) {
	t.Helper()
	reg, liveAddr := newFakeRegistryServer(t)
	deadAddr := reservedDeadAddr(t)

	for _, op := range registryOps {
		t.Run(op.name, func(t *testing.T) {
			// A fresh client per op avoids cross-op backoff state building up
			// against the always-failing dead address (reghttp tracks
			// per-host backoff on the client itself, and it compounds across
			// requests sharing one regclient.RegClient instance).
			var mc *MirrorClient
			if primarySucceeds {
				mc = splitClient(symbolicRegistryHost, liveAddr, deadAddr)
			} else {
				mc = splitClient(symbolicRegistryHost, deadAddr, liveAddr)
			}
			op.run(t, mc, reg, symbolicRegistryHost)
		})
	}
}

func TestClientOps_PrimarySucceeds(t *testing.T) {
	runRegistryOps(t, true)
}

func TestClientOps_FallbackSucceeds(t *testing.T) {
	runRegistryOps(t, false)
}

// TestCopyImageWith_SignatureCopy exercises the digest-based cosign .sig-tag
// copy in copyImageWith: a digest-only source (no tag) triggers a best-effort
// copy of the "sha256-<hex>.sig" tag alongside the main manifest.
func TestCopyImageWith_SignatureCopy(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	digest, _ := seedTestImage(t, reg, "repo-copy-src", "v1")
	sigTag := "sha256-" + strings.TrimPrefix(digest, "sha256:") + ".sig"
	seedTestImage(t, reg, "repo-copy-src", sigTag)

	mc := NewMirrorClient([]string{host}, "")
	src := host + "/repo-copy-src@" + digest
	dest := host + "/repo-copy-dst:v1"

	effectiveDest, err := mc.copyImageWith(context.Background(), mc.rc, src, dest)
	if err != nil {
		t.Fatalf("copyImageWith: %v", err)
	}
	if effectiveDest == "" {
		t.Error("expected a non-empty effective destination")
	}

	reg.mu.Lock()
	_, sigCopied := reg.manifests["repo-copy-dst:"+sigTag]
	reg.mu.Unlock()
	if !sigCopied {
		t.Error("expected the cosign signature tag to be copied to the destination")
	}
}

// TestCopyImageWith_DigestTagSynthesis exercises the "synthesise a tag from
// the source digest" branch: both source and destination are digest-only
// references (no explicit tag), so copyImageWith must invent a destination
// tag from the source digest before pushing.
func TestCopyImageWith_DigestTagSynthesis(t *testing.T) {
	reg, host := newFakeRegistryServer(t)
	digest, _ := seedTestImage(t, reg, "repo-copy-synth-src", "v1")

	mc := NewMirrorClient([]string{host}, "")
	src := host + "/repo-copy-synth-src@" + digest
	dest := host + "/repo-copy-synth-dst@" + digest

	effectiveDest, err := mc.copyImageWith(context.Background(), mc.rc, src, dest)
	if err != nil {
		t.Fatalf("copyImageWith: %v", err)
	}
	wantTag := "repo-copy-synth-dst:" + strings.Replace(digest, ":", "-", 1)
	if !strings.Contains(effectiveDest, wantTag) {
		t.Errorf("expected effective destination to contain synthesized tag %q, got %q", wantTag, effectiveDest)
	}
}
