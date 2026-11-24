package worker

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/manifest"
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
	var writes atomic.Int64
	handler := registry.New()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	pub := scannerDBPublisher{source: source.URL + "/scanner.zip", dest: dest, registry: NewMirrorClient(true, dest.CommonName())}
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
}
