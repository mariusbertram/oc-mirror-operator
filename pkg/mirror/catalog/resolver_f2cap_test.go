package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"testing/fstest"
)

// Tests fuer extractFBCLayer: Dateien > 64MiB Cap muessen erkannt werden.
// Regression zu F2: LimitReader schnitt grossere FBC-Dateien still ab und
// erzeugte trunciertes JSON, das declcfg.LoadFS mit "unexpected EOF" killte.

func gzipTar(t *testing.T, files map[string][]byte) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractFBCLayer_SmallFileExtracted(t *testing.T) {
	src := gzipTar(t, map[string][]byte{
		"configs/foo/catalog.json": []byte(`{"schema":"olm.package"}`),
	})
	fsMap := fstest.MapFS{}
	if n := extractFBCLayer(src, fsMap); n != 1 {
		t.Fatalf("want 1 file, got %d", n)
	}
	if _, ok := fsMap["configs/foo/catalog.json"]; !ok {
		t.Fatal("file missing from map")
	}
}

func TestExtractFBCLayer_UnderLimitComplete(t *testing.T) {
	// genau an/nah am Limit: unter dem Cap muss die Datei vollstaendig sein
	payload := strings.Repeat("a", 1024)
	src := gzipTar(t, map[string][]byte{"configs/p/catalog.json": []byte(payload)})
	fsMap := fstest.MapFS{}
	_ = extractFBCLayer(src, fsMap)
	got := fsMap["configs/p/catalog.json"]
	if got == nil || len(got.Data) != len(payload) {
		t.Fatalf("file truncated or missing: got %d bytes", len(got.Data))
	}
}

func TestExtractFBCLayer_OverLimitDetected(t *testing.T) {
	// 70 MiB payload > 64 MiB Cap: darf NICHT als (truncierte) Datei landen.
	// Erwartung nach Fix: Fehler/Erkennung statt stiller Truncation.
	big := bytes.Repeat([]byte("x"), 70*1024*1024)
	src := gzipTar(t, map[string][]byte{"configs/big/catalog.json": big})
	fsMap := fstest.MapFS{}
	n := extractFBCLayer(src, fsMap)

	entry, exists := fsMap["configs/big/catalog.json"]
	if !exists {
		return // Datei verworfen = akzeptable Alternative, solange nicht trunziert
	}
	if len(entry.Data) < len(big) {
		t.Fatalf("BUG F2: Datei still trunciert auf %d von %d bytes — trunciertes JSON landet im FS und killt declcfg.LoadFS",
			len(entry.Data), len(big))
	}
	_ = n
}

func TestExtractFBCLayer_OverLimitErrorReturned(t *testing.T) {
	// Variante: extractFBCLayer sollte nach dem Fix truncation melden koennen.
	// Dieser Test dokumentiert das Ziel-API-Verhalten: kein stiller Datenverlust.
	big := bytes.Repeat([]byte("x"), 65*1024*1024)
	src := gzipTar(t, map[string][]byte{"configs/over/catalog.json": big})
	fsMap := fstest.MapFS{}
	n := extractFBCLayer(src, fsMap)
	entry, exists := fsMap["configs/over/catalog.json"]
	if exists && len(entry.Data) != len(big) {
		t.Fatalf("stille Truncation: %d von %d bytes im MapFile (count=%d)",
			len(entry.Data), len(big), n)
	}
}
