package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

func scannerZIP(t *testing.T, contents string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	entry, err := w.Create("definitions")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestScannerDBPublication(t *testing.T) {
	var writes, uploads atomic.Int64
	handler := registry.New()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploads.Add(1)
		}
		switch r.Method {
		case http.MethodPut, http.MethodPost, http.MethodPatch:
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer target.Close()

	type response struct {
		body []byte
		etag string
	}
	var bundle atomic.Pointer[response]
	bundle.Store(&response{scannerZIP(t, "one"), `"1"`})
	var conditional atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := bundle.Load()
		w.Header().Set("ETag", b.etag)
		if r.Header.Get("If-None-Match") == b.etag {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(b.body)
	}))
	defer source.Close()

	dest, err := ref.New(strings.TrimPrefix(target.URL, "http://") + "/mirror/rhacs/scanner-db:4.11.0")
	if err != nil {
		t.Fatal(err)
	}
	pub := scannerDBPublisher{source: source.URL + "/scanner.zip", dest: dest, registry: newPlainHTTPRegistry(dest.Registry)}
	ctx := t.Context()
	check := func() {
		t.Helper()
		if err := pub.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check()
	initial := pub.published
	if writes.Load() == 0 {
		t.Fatal("expected initial upload")
	}
	writes.Store(0)
	check()
	if conditional.Load() != 1 || writes.Load() != 0 {
		t.Fatal("unchanged bundle should use conditional GET and skip registry writes")
	}
	pub.etag, pub.published = "", "" // fresh process, same registry
	check()
	if writes.Load() != 0 || pub.published != initial {
		t.Fatal("restart re-uploaded identical database")
	}

	bundle.Store(&response{[]byte("upstream error page"), `"2"`})
	if err := pub.sync(ctx); err == nil || writes.Load() != 0 {
		t.Fatal("invalid archive should fail without changing the published tag")
	}
	var empty bytes.Buffer
	if err := zip.NewWriter(&empty).Close(); err != nil {
		t.Fatal(err)
	}
	bundle.Store(&response{empty.Bytes(), `"2"`})
	if err := pub.sync(ctx); err == nil || writes.Load() != 0 {
		t.Fatal("empty ZIP should leave the existing tag intact")
	}
	updated := scannerZIP(t, "two")
	bundle.Store(&response{updated, `"2"`})
	check()
	if pub.published == initial || writes.Load() == 0 {
		t.Fatal("expected updated manifest")
	}
	m, err := pub.registry.ManifestGet(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := m.(manifest.Imager).GetLayers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("expected one layer, got %v (%v)", layers, err)
	}
	layer := layers[0]
	if layer.MediaType != "application/zip" || layer.Digest != digest.FromBytes(updated) || layer.Annotations["org.opencontainers.image.title"] != "scanner.zip" {
		t.Fatalf("unexpected layer: %+v", layer)
	}

	// A second tag for identical content in this repository only needs a manifest.
	pub.dest = dest.SetTag("another-tag")
	uploads.Store(0)
	writes.Store(0)
	check()
	if uploads.Load() != 0 || writes.Load() != 1 {
		t.Fatalf("expected only a manifest PUT, got %d writes (%d uploads)", writes.Load(), uploads.Load())
	}
}

func TestScannerDBMainInvalidArgs(t *testing.T) {
	for _, args := range [][]string{{"--bad-flag"}, {}, {"--registry", "bad host"}} {
		if code := ScannerDBMain(t.Context(), args); code != 1 {
			t.Errorf("args %v: exit %d, want 1", args, code)
		}
	}
}

func TestScannerDBRun(t *testing.T) {
	for _, tc := range []struct{ version, filename, tag string }{
		{"4.11.0", "/4.11.0/scanner-vulns-4.11.0.zip", "4.11.0"},
		{"", "/scanner-vuln-updates.zip", "latest"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()
			body := scannerZIP(t, "definitions")
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.filename {
					t.Errorf("unexpected bundle path %q", r.URL.Path)
				}
				_, _ = w.Write(body)
			}))
			defer source.Close()
			handler := registry.New()
			published := make(chan struct{}, 1)
			reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r)
				if r.Method == http.MethodPut && r.URL.Path == "/v2/rhacs/scanner-db/manifests/"+tc.tag {
					published <- struct{}{}
				}
			}))
			defer reg.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			done := make(chan int, 1)
			go func() {
				args := []string{"--registry", strings.TrimPrefix(reg.URL, "http://"), "--version", tc.version, "--insecure"}
				done <- runScannerDB(ctx, args, source.URL+"/")
			}()
			select {
			case <-published:
			case <-ctx.Done():
				t.Fatal("publisher did not push the expected tag")
			}
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("publisher exited with %d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("publisher did not stop on cancellation")
			}
		})
	}
}

func TestScannerDBDownloadFailure(t *testing.T) {
	reg := httptest.NewServer(registry.New())
	defer reg.Close()
	dest, err := ref.New(strings.TrimPrefix(reg.URL, "http://") + "/mirror/scanner:latest")
	if err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.NotFoundHandler())
	defer source.Close()
	pub := scannerDBPublisher{source: source.URL, dest: dest, registry: NewMirrorClient(true, dest.CommonName())}
	if err := pub.sync(t.Context()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing bundle: %v", err)
	}
	pub.source = "://"
	if err := pub.sync(t.Context()); err == nil {
		t.Fatal("expected malformed source error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := pub.sync(ctx); err == nil || !strings.Contains(err.Error(), "destination tag") {
		t.Fatalf("canceled registry request: %v", err)
	}
}

func TestScannerDBStagingFailures(t *testing.T) {
	pub := scannerDBPublisher{}
	if _, err := pub.publish(t.Context(), iotest.ErrReader(io.ErrUnexpectedEOF), "scanner.zip", "", -1); err == nil {
		t.Fatal("expected interrupted download to fail")
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := pub.publish(t.Context(), strings.NewReader(""), "scanner.zip", "", 0); err == nil {
		t.Fatal("expected unavailable scratch directory to fail")
	}
}

// logCapture records publisher log lines; the progress goroutine writes
// concurrently with the test reading them.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCapture) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := strings.Join(c.lines, "\n")
	c.lines = nil
	return out
}

func TestScannerDBLogging(t *testing.T) {
	// Slow transfers down so the progress ticker fires during each of them.
	handler := registry.New()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut || r.Method == http.MethodPatch {
			time.Sleep(50 * time.Millisecond)
		}
		handler.ServeHTTP(w, r)
	}))
	defer target.Close()
	body := scannerZIP(t, "definitions")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"1"`)
		if r.Header.Get("If-None-Match") == `"1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write(body)
	}))
	defer source.Close()

	dest, err := ref.New(strings.TrimPrefix(target.URL, "http://") + "/mirror/rhacs/scanner-db:4.11.0")
	if err != nil {
		t.Fatal(err)
	}
	var logs logCapture
	pub := scannerDBPublisher{
		source: source.URL + "/scanner.zip", dest: dest, registry: newPlainHTTPRegistry(dest.Registry),
		logf: logs.logf, progressInterval: 5 * time.Millisecond,
	}
	expect := func(step string, want ...string) {
		t.Helper()
		if err := pub.sync(t.Context()); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		got := logs.take()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in log:\n%s", step, w, got)
			}
		}
	}

	expect("initial publication",
		"Checking scanner DB for "+dest.CommonName(),
		"Downloading "+source.URL+"/scanner.zip ("+formatBytes(int64(len(body)))+")",
		"Downloading scanner DB: ",
		"Downloaded "+formatBytes(int64(len(body)))+" (sha256:",
		"Uploading scanner DB layer",
		"Uploading scanner DB: ",
		"Published scanner DB manifest sha256:",
	)
	expect("unchanged upstream", `Scanner DB unchanged upstream (ETag "1")`)
	pub.etag, pub.published = "", ""
	expect("restart", "Destination already serves this scanner DB, skipping upload")
	pub.dest = dest.SetTag("another-tag")
	expect("new tag", "already in the destination repository, publishing the manifest only")
}

func TestScannerDBDownloadErrorReportsProgress(t *testing.T) {
	var logs logCapture
	pub := scannerDBPublisher{logf: logs.logf}
	_, err := pub.publish(t.Context(), io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(io.ErrUnexpectedEOF)), "scanner.zip", "", 2048)
	if err == nil || !strings.Contains(err.Error(), "after 7 B of 2.0 KiB, 0%") {
		t.Fatalf("expected transferred bytes in error, got %v", err)
	}
}

func TestFormatProgress(t *testing.T) {
	for _, tc := range []struct {
		done, total int64
		want        string
	}{
		{-1, -1, "size unknown"},
		{-1, 595501189, "567.9 MiB"},
		{512, -1, "512 B"},
		{1545897107, 0, "1.4 GiB"},
		{297750594, 595501189, "284.0 MiB of 567.9 MiB, 49%"},
	} {
		if got := formatProgress(tc.done, tc.total); got != tc.want {
			t.Errorf("formatProgress(%d, %d) = %q, want %q", tc.done, tc.total, got, tc.want)
		}
	}
}

func TestProgressReaderSeek(t *testing.T) {
	r := &progressReader{r: strings.NewReader("abcdef")}
	if _, err := io.ReadAll(r); err != nil || r.n.Load() != 6 {
		t.Fatalf("read %d bytes, err %v", r.n.Load(), err)
	}
	if pos, err := r.Seek(2, io.SeekStart); err != nil || pos != 2 || r.n.Load() != 2 {
		t.Fatalf("seek: pos %d, count %d, err %v", pos, r.n.Load(), err)
	}
	if _, err := (&progressReader{r: iotest.ErrReader(io.EOF)}).Seek(0, io.SeekStart); err == nil {
		t.Fatal("expected error for a non-seekable reader")
	}
}

func TestScannerDBRunMultipleVersions(t *testing.T) {
	var requested sync.Map
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, true)
		_, _ = w.Write(scannerZIP(t, r.URL.Path))
	}))
	defer source.Close()
	handler := registry.New()
	var mu sync.Mutex
	tags := map[string]bool{}
	published := make(chan struct{}, 4)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if tag, ok := strings.CutPrefix(r.URL.Path, "/v2/rhacs/scanner-db/manifests/"); ok && r.Method == http.MethodPut {
			mu.Lock()
			tags[tag] = true
			mu.Unlock()
			published <- struct{}{}
		}
	}))
	defer reg.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		args := []string{"--registry", strings.TrimPrefix(reg.URL, "http://"), "--version", "4.10.3", "--version", "4.11.0", "--version", "4.10.3", "--insecure"}
		done <- runScannerDB(ctx, args, source.URL+"/")
	}()
	for range 2 {
		select {
		case <-published:
		case <-ctx.Done():
			t.Fatal("publisher did not push both versions")
		}
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("publisher exited with %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tags) != 2 || !tags["4.10.3"] || !tags["4.11.0"] {
		t.Fatalf("published tags %v, want 4.10.3 and 4.11.0", tags)
	}
	for _, p := range []string{"/4.10.3/scanner-vulns-4.10.3.zip", "/4.11.0/scanner-vulns-4.11.0.zip"} {
		if _, ok := requested.Load(p); !ok {
			t.Errorf("bundle %s was not downloaded", p)
		}
	}
}

func TestScannerDBRemovesSupersededManifest(t *testing.T) {
	var rejectDeletes, hideTags, hideDigests atomic.Bool
	handler := registry.New()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && rejectDeletes.Load():
			http.Error(w, `{"errors":[{"code":"UNSUPPORTED"}]}`, http.StatusMethodNotAllowed)
		case strings.HasSuffix(r.URL.Path, "/tags/list") && hideTags.Load(),
			r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/sha256:") && hideDigests.Load():
			http.NotFound(w, r)
		default:
			handler.ServeHTTP(w, r)
		}
	}))
	defer target.Close()
	var bundle atomic.Pointer[[]byte]
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(*bundle.Load())
	}))
	defer source.Close()

	dest, err := ref.New(strings.TrimPrefix(target.URL, "http://") + "/mirror/rhacs/scanner-db:4.11.0")
	if err != nil {
		t.Fatal(err)
	}
	var logs logCapture
	pub := scannerDBPublisher{
		source: source.URL + "/scanner.zip", dest: dest, registry: newPlainHTTPRegistry(dest.Registry), logf: logs.logf,
	}
	ctx := t.Context()
	generation := 0
	update := func(step, want string) digest.Digest {
		t.Helper()
		generation++
		b := scannerZIP(t, fmt.Sprint("generation ", generation))
		bundle.Store(&b)
		previous := pub.published
		if err := pub.sync(ctx); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got := logs.take(); !strings.Contains(got, want) {
			t.Errorf("%s: missing %q in log:\n%s", step, want, got)
		}
		return previous
	}
	exists := func(d digest.Digest) bool {
		t.Helper()
		_, err := pub.registry.ManifestHead(ctx, dest.SetDigest(d.String()))
		return err == nil
	}

	update("initial publication", "Published scanner DB manifest")
	old := update("refresh", "Deleted superseded scanner DB manifest")
	if exists(old) {
		t.Error("superseded manifest still exists")
	}

	// Another tag on the current manifest must survive the next refresh.
	pinned, err := pub.registry.ManifestGet(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.registry.ManifestPut(ctx, dest.SetTag("pinned"), pinned); err != nil {
		t.Fatal(err)
	}
	old = update("refresh with a second tag", "still tagged as pinned")
	if !exists(old) {
		t.Error("manifest referenced by another tag was deleted")
	}

	// A manifest someone else pushed to the tag is never deleted.
	foreign, err := manifest.New(manifest.WithOrig(v1.Manifest{
		Versioned: v1.ManifestSchemaVersion, MediaType: mediatype.OCI1Manifest, ArtifactType: "application/vnd.example.other",
		Config: descriptor.Descriptor{MediaType: mediatype.OCI1Empty, Digest: descriptor.EmptyDigest, Size: int64(len(descriptor.EmptyData))},
		Layers: []descriptor.Descriptor{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.registry.ManifestPut(ctx, dest, foreign); err != nil {
		t.Fatal(err)
	}
	update("refresh over a foreign manifest", "not published by the scanner DB publisher")
	if !exists(foreign.GetDescriptor().Digest) {
		t.Error("foreign manifest was deleted")
	}

	rejectDeletes.Store(true)
	old = update("registry without deletes", "registry garbage collection must reclaim it")
	if !exists(old) || pub.published == old {
		t.Error("a rejected delete must keep the old manifest and still publish the new one")
	}
	rejectDeletes.Store(false)

	// Without a tag listing the publisher cannot prove the digest is unshared.
	hideTags.Store(true)
	if old = update("tag listing unavailable", "list tags:"); !exists(old) {
		t.Error("manifest deleted without checking the other tags")
	}
	hideTags.Store(false)

	hideDigests.Store(true)
	update("previous manifest unreadable", "read manifest:")
	hideDigests.Store(false)
}

// plainHTTPRegistry talks plain HTTP to a test registry without the insecure
// MirrorClient's HTTPS fallback. That fallback fails against an httptest
// server, and regclient's per-host backoff then grows with every expected
// 4xx answer, adding up to 30 s to each request this test provokes.
type plainHTTPRegistry struct{ rc *regclient.RegClient }

func newPlainHTTPRegistry(host string) plainHTTPRegistry {
	return plainHTTPRegistry{regclient.New(regclient.WithConfigHost(config.Host{Name: host, TLS: config.TLSDisabled}))}
}

func (p plainHTTPRegistry) ManifestHead(ctx context.Context, r ref.Ref) (manifest.Manifest, error) {
	return p.rc.ManifestHead(ctx, r)
}

func (p plainHTTPRegistry) ManifestGet(ctx context.Context, r ref.Ref) (manifest.Manifest, error) {
	return p.rc.ManifestGet(ctx, r)
}

func (p plainHTTPRegistry) ManifestPut(ctx context.Context, r ref.Ref, m manifest.Manifest) error {
	return p.rc.ManifestPut(ctx, r, m)
}

func (p plainHTTPRegistry) BlobExists(ctx context.Context, r ref.Ref, d descriptor.Descriptor) bool {
	br, err := p.rc.BlobHead(ctx, r, d)
	if err == nil {
		_ = br.Close()
	}
	return err == nil
}

func (p plainHTTPRegistry) BlobPut(ctx context.Context, r ref.Ref, d descriptor.Descriptor, rdr io.Reader) (descriptor.Descriptor, error) {
	return p.rc.BlobPut(ctx, r, d, rdr)
}

func (p plainHTTPRegistry) TagList(ctx context.Context, r ref.Ref) ([]string, error) {
	tl, err := p.rc.TagList(ctx, r)
	if err != nil {
		return nil, err
	}
	return tl.GetTags()
}

func (p plainHTTPRegistry) DeleteManifest(ctx context.Context, image string) error {
	r, err := ref.New(image)
	if err != nil {
		return err
	}
	return p.rc.ManifestDelete(ctx, r)
}
