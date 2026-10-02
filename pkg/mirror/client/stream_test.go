package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/config"
)

// quayLikeRegistry is a destination registry that implements the blob upload
// session semantics of Quay (endpoints/v2/blob.py) on top of fakeRegistry:
//
//   - PATCH appends a chunk to the session; a failed chunk leaves the session
//     intact and the client resumes at the offset reported by GET.
//   - A monolithic PUT that fails part way cancels the session
//     (complete_when_uploaded), so any retry on it gets BLOB_UPLOAD_UNKNOWN.
//
// failMonolithicOver emulates a long-running monolithic PUT being cut off (a
// route timeout or dropped connection): any PUT with a body larger than it
// fails after reading part of the body.
type quayLikeRegistry struct {
	*fakeRegistry

	mu                 sync.Mutex
	nextID             int
	sessions           map[string]*bytes.Buffer
	failMonolithicOver int64
	failPatchNo        int // fail the n-th PATCH (1-based) once; 0 disables

	patches       int
	maxPatch      int
	monolithicPut map[string]bool // digest → pushed via monolithic PUT
}

func newQuayLikeRegistryServer(t *testing.T) (*quayLikeRegistry, string) {
	t.Helper()
	q := &quayLikeRegistry{
		fakeRegistry:  newFakeRegistryHandler(),
		sessions:      map[string]*bytes.Buffer{},
		monolithicPut: map[string]bool{},
	}
	srv := httptest.NewServer(q)
	t.Cleanup(srv.Close)
	return q, strings.TrimPrefix(srv.URL, "http://")
}

func (q *quayLikeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.Contains(path, uploadsSegment) {
		q.fakeRegistry.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost {
		q.mu.Lock()
		q.nextID++
		id := "u" + strconv.Itoa(q.nextID)
		q.sessions[id] = &bytes.Buffer{}
		q.mu.Unlock()
		w.Header().Set("Location", path+id)
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := path[strings.LastIndex(path, "/")+1:]
	q.mu.Lock()
	sess, ok := q.sessions[id]
	q.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UPLOAD_UNKNOWN","message":"blob upload unknown to registry"}]}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		q.writeRange(w, path, sess)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		q.patch(w, r, path, sess)
	case http.MethodPut:
		q.put(w, r, id, sess)
	default:
		http.NotFound(w, r)
	}
}

func (q *quayLikeRegistry) writeRange(w http.ResponseWriter, path string, sess *bytes.Buffer) {
	q.mu.Lock()
	n := sess.Len()
	q.mu.Unlock()
	w.Header().Set("Location", path)
	w.Header().Set("Range", fmt.Sprintf("0-%d", n-1))
}

func (q *quayLikeRegistry) patch(w http.ResponseWriter, r *http.Request, path string, sess *bytes.Buffer) {
	var start, end int
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "%d-%d", &start, &end); err != nil {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(r.Body)
	q.mu.Lock()
	q.patches++
	if len(body) > q.maxPatch {
		q.maxPatch = len(body)
	}
	fail := q.patches == q.failPatchNo
	if fail {
		q.failPatchNo = 0
	}
	cur := sess.Len()
	if !fail && start == cur && end-start+1 == len(body) {
		sess.Write(body)
	}
	q.mu.Unlock()
	switch {
	case fail:
		http.Error(w, "storage hiccup", http.StatusInternalServerError)
	case start != cur:
		q.writeRange(w, path, sess)
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	default:
		q.writeRange(w, path, sess)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (q *quayLikeRegistry) put(w http.ResponseWriter, r *http.Request, id string, sess *bytes.Buffer) {
	digest := r.URL.Query().Get("digest")
	if q.failMonolithicOver > 0 && r.ContentLength > q.failMonolithicOver {
		_, _ = io.CopyN(io.Discard, r.Body, q.failMonolithicOver/2)
		q.mu.Lock()
		delete(q.sessions, id) // Quay cancels the upload when the PUT fails
		q.mu.Unlock()
		http.Error(w, "upstream request timeout", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	q.mu.Lock()
	defer q.mu.Unlock()
	data := append(sess.Bytes(), body...)
	delete(q.sessions, id)
	if sha256Digest(data) != digest {
		http.Error(w, "digest mismatch", http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		q.monolithicPut[digest] = true
	}
	q.fakeRegistry.mu.Lock()
	q.blobs[digest] = data
	q.fakeRegistry.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

// seedImageWithLayer seeds a single-layer OCI image with the given layer
// content under repo:tag and returns the layer digest.
func seedImageWithLayer(reg *fakeRegistry, repo, tag string, layer []byte) string {
	layerDigest := reg.seedBlob(layer)
	configJSON := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["%s"]}}`, layerDigest))
	configDigest := reg.seedBlob(configJSON)
	manifestJSON := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		ociManifestMediaType, ociConfigMediaType, configDigest, len(configJSON),
		ociLayerMediaType, layerDigest, len(layer),
	))
	reg.seedManifest(repo, tag, ociManifestMediaType, manifestJSON)
	return layerDigest
}

// streamFixture lowers the blob size limits so a small test layer counts as
// "large", points blobBufferDir at a temp dir, and seeds a source image.
type streamFixture struct {
	dest      *quayLikeRegistry
	mc        *MirrorClient
	src, dst  string
	layer     []byte
	layerDig  string
	bufferDir string
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	origThreshold, origChunk, origDir := largeBlobThreshold, streamChunkSize, blobBufferDir
	t.Cleanup(func() {
		largeBlobThreshold, streamChunkSize, blobBufferDir = origThreshold, origChunk, origDir
	})
	largeBlobThreshold = 4 * 1024
	streamChunkSize = 3 * 1024
	blobBufferDir = t.TempDir()

	layer := make([]byte, 20*1024+123)
	if _, err := rand.Read(layer); err != nil {
		t.Fatal(err)
	}
	srcReg, srcHost := newFakeRegistryServer(t)
	layerDig := seedImageWithLayer(srcReg, "ocp/release", "v1", layer)

	dest, destHost := newQuayLikeRegistryServer(t)
	mc := NewMirrorClient([]string{srcHost, destHost}, "", destHost)
	return &streamFixture{
		dest:      dest,
		mc:        mc,
		src:       srcHost + "/ocp/release:v1",
		dst:       destHost + "/mirror/release:v1",
		layer:     layer,
		layerDig:  layerDig,
		bufferDir: blobBufferDir,
	}
}

func (f *streamFixture) assertMirrored(t *testing.T) {
	t.Helper()
	f.dest.fakeRegistry.mu.Lock()
	got := f.dest.blobs[f.layerDig]
	f.dest.fakeRegistry.mu.Unlock()
	if !bytes.Equal(got, f.layer) {
		t.Fatalf("destination layer has %d bytes, want %d identical bytes", len(got), len(f.layer))
	}
	exists, err := f.mc.CheckExist(context.Background(), f.dst)
	if err != nil || !exists {
		t.Fatalf("destination manifest missing: exists=%v err=%v", exists, err)
	}
	entries, err := os.ReadDir(f.bufferDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("blob buffer dir not empty: %v", entries)
	}
}

func TestCopyImage_StreamsLargeBlobsChunked(t *testing.T) {
	f := newStreamFixture(t)
	// A monolithic PUT of the large layer would be cut off and its upload
	// session cancelled, like a long-running streamed PUT against Quay.
	f.dest.failMonolithicOver = largeBlobThreshold

	if _, err := f.mc.CopyImage(context.Background(), f.src, f.dst); err != nil {
		t.Fatalf("CopyImage: %v", err)
	}
	f.assertMirrored(t)

	if f.dest.monolithicPut[f.layerDig] {
		t.Fatal("large layer was pushed with a monolithic PUT, want chunked")
	}
	wantPatches := (len(f.layer) + int(streamChunkSize) - 1) / int(streamChunkSize)
	if f.dest.patches != wantPatches {
		t.Fatalf("PATCH requests = %d, want %d", f.dest.patches, wantPatches)
	}
	if f.dest.maxPatch > int(streamChunkSize) {
		t.Fatalf("largest PATCH = %d bytes, want <= %d", f.dest.maxPatch, streamChunkSize)
	}
	if len(f.dest.monolithicPut) == 0 {
		t.Fatal("small blobs (config) should still use a monolithic PUT")
	}
}

func TestCopyImage_ResumesAfterFailedChunk(t *testing.T) {
	f := newStreamFixture(t)
	f.dest.failPatchNo = 3

	if _, err := f.mc.CopyImage(context.Background(), f.src, f.dst); err != nil {
		t.Fatalf("CopyImage: %v", err)
	}
	f.assertMirrored(t)
	if f.dest.failPatchNo != 0 {
		t.Fatal("the injected chunk failure was never hit")
	}
}

func TestCopyImageBuffered_UsesMonolithicPutFromDisk(t *testing.T) {
	f := newStreamFixture(t)

	if _, err := f.mc.CopyImageBuffered(context.Background(), f.src, f.dst); err != nil {
		t.Fatalf("CopyImageBuffered: %v", err)
	}
	f.assertMirrored(t)
	if !f.dest.monolithicPut[f.layerDig] || f.dest.patches != 0 {
		t.Fatalf("large layer: monolithic=%v patches=%d, want monolithic PUT and no PATCH",
			f.dest.monolithicPut[f.layerDig], f.dest.patches)
	}
	if bc := f.mc.bufferedClient(); bc == f.mc || bc != f.mc.bufferedClient() {
		t.Fatal("buffered client must be a single, separate client")
	}
}

// TestCopyImageBuffered_ReproducesSessionLossOnFailedMonolithicPut documents
// why large blobs must not be streamed as one monolithic PUT: once that PUT is
// cut off, Quay has cancelled the session and regclient's chunked fallback on
// it fails with 404 BLOB_UPLOAD_UNKNOWN. The buffered strategy only avoids
// this because its PUT from local disk finishes quickly in practice.
func TestCopyImageBuffered_ReproducesSessionLossOnFailedMonolithicPut(t *testing.T) {
	f := newStreamFixture(t)
	f.dest.failMonolithicOver = largeBlobThreshold

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.mc.CopyImageBuffered(ctx, f.src, f.dst)
	if err == nil {
		t.Fatal("CopyImageBuffered succeeded, want the cancelled upload session to fail it")
	}
	if !strings.Contains(err.Error(), "failed to send blob (chunk)") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want a 404 on the chunked fallback", err)
	}
}

func TestBufferedClient_DefaultsToSelf(t *testing.T) {
	mc := &MirrorClient{}
	if mc.bufferedClient() != mc {
		t.Fatal("a client not built by NewMirrorClient must buffer with itself")
	}
}

func TestDestHostConfig(t *testing.T) {
	stream := destHostConfig("reg.io", config.TLSDisabled, false)
	if stream.BlobMax != largeBlobThreshold || stream.BlobChunk != streamChunkSize || stream.TLS != config.TLSDisabled {
		t.Fatalf("streaming host config = %+v", stream)
	}
	mono := destHostConfig("reg.io", config.TLSInsecure, true)
	if mono.BlobMax != -1 || mono.BlobChunk != 0 || mono.TLS != config.TLSInsecure {
		t.Fatalf("monolithic host config = %+v", mono)
	}
	// Quay assembles chunks server-side only when every part meets S3's
	// 5 MiB multipart minimum.
	if streamChunkSize < 5*1024*1024 || streamChunkSize > largeBlobThreshold {
		t.Fatalf("streamChunkSize = %d, want >= 5 MiB and <= largeBlobThreshold", streamChunkSize)
	}
}
