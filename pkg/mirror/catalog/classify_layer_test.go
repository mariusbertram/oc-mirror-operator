package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	size     int64
	body     []byte
}

func makeGzipTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Typeflag: e.typeflag,
			Name:     e.name,
			Size:     e.size,
			Mode:     0o644,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if e.typeflag == tar.TypeReg && e.size > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	return buf.Bytes()
}

func TestClassifyLayer_OnlyConfigs(t *testing.T) {
	body := []byte("schema: olm.package\nname: foo\n")
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
		{name: "configs/foo/", typeflag: tar.TypeDir},
		{name: "configs/foo/catalog.yaml", typeflag: tar.TypeReg, size: int64(len(body)), body: body},
	})
	skip, sz, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !skip {
		t.Fatalf("expected skippable=true")
	}
	if sz != int64(len(body)) {
		t.Fatalf("expected size=%d got %d", len(body), sz)
	}
}

func TestClassifyLayer_ConfigsAndCache(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/foo/catalog.yaml", typeflag: tar.TypeReg, size: 3, body: []byte("foo")},
		{name: "tmp/cache/db.pogreb", typeflag: tar.TypeReg, size: 4, body: []byte("data")},
	})
	skip, _, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil || !skip {
		t.Fatalf("expected skip+nil err, got skip=%v err=%v", skip, err)
	}
}

func TestClassifyLayer_HasOtherContent(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/foo/catalog.yaml", typeflag: tar.TypeReg, size: 3, body: []byte("foo")},
		{name: "etc/hosts", typeflag: tar.TypeReg, size: 4, body: []byte("data")},
	})
	skip, _, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if skip {
		t.Fatalf("expected skip=false because /etc/hosts is outside known FBC paths")
	}
}

func TestClassifyLayer_OnlyDirsNotSkippable(t *testing.T) {
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/", typeflag: tar.TypeDir},
	})
	skip, _, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if skip {
		t.Fatalf("layer with only directory entries should not be skipped (no payload to replace)")
	}
}

func TestClassifyLayer_SymlinkInsideConfigsNotSkippable(t *testing.T) {
	// Symlinks could point outside configs/ — refuse to drop.
	data := makeGzipTar(t, []tarEntry{
		{name: "configs/foo/link", typeflag: tar.TypeSymlink},
	})
	skip, _, _, err := classifyLayer(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if skip {
		t.Fatalf("layer containing symlinks must not be classified as skippable")
	}
}

func TestClassifyLayer_NotGzip(t *testing.T) {
	if _, _, _, err := classifyLayer(bytes.NewReader([]byte("not gzip"))); err == nil {
		t.Fatalf("expected gzip error")
	}
}
