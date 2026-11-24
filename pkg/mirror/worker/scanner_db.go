package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/errs"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

// ScannerDBMain runs the dedicated publisher for a MirrorTarget.
// The deployment's Recreate strategy keeps one publisher per target.
func ScannerDBMain(ctx context.Context, args []string) int {
	return runScannerDB(ctx, args, "https://install.stackrox.io/scanner/")
}

func runScannerDB(ctx context.Context, args []string, baseURL string) int {
	fs := flag.NewFlagSet("acs-scanner-db", flag.ContinueOnError)
	registry := fs.String("registry", "", "Target registry, optionally with a path prefix")
	var versions []string
	fs.Func("version", "Published RHACS bundle version; repeat for several, empty or omitted for legacy Scanner", func(v string) error {
		versions = append(versions, v)
		return nil
	})
	insecure := fs.Bool("insecure", false, "Allow insecure target registry")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *registry == "" {
		oclog.Println("Scanner DB publishers require --registry")
		return 1
	}
	if len(versions) == 0 {
		versions = []string{""}
	}

	var pubs []*scannerDBPublisher
	seen := map[string]bool{}
	for _, version := range versions {
		if seen[version] {
			continue
		}
		seen[version] = true
		tag, filename := "latest", "scanner-vuln-updates.zip"
		if version != "" {
			tag, filename = version, fmt.Sprintf("%[1]s/scanner-vulns-%[1]s.zip", version)
		}
		dest, err := ref.New(strings.TrimSuffix(*registry, "/") + "/rhacs/scanner-db:" + tag)
		if err != nil {
			oclog.Printf("Invalid scanner DB destination: %v", err)
			return 1
		}
		pub := &scannerDBPublisher{
			source: baseURL + filename, dest: dest,
			registry: NewMirrorClient(*insecure, dest.CommonName()),
		}
		oclog.Printf("Scanner DB publisher started: %s -> %s, checking every %s", pub.source, dest.CommonName(), scannerDBInterval)
		pubs = append(pubs, pub)
	}

	ticker := time.NewTicker(scannerDBInterval)
	defer ticker.Stop()
	next := time.Now().Add(scannerDBInterval)
	for ctx.Err() == nil {
		// One bundle at a time: the pod's ephemeral storage holds one ZIP.
		for _, pub := range pubs {
			if ctx.Err() != nil {
				break
			}
			syncCtx, cancel := context.WithTimeout(ctx, scannerDBInterval)
			err := pub.sync(syncCtx)
			cancel()
			if err != nil {
				oclog.Printf("Scanner DB sync to %s failed: %v", pub.dest.CommonName(), err)
			} else {
				oclog.Printf("Scanner DB is current at %s@%s", pub.dest.CommonName(), pub.published)
			}
		}
		oclog.Printf("Next scanner DB check at %s", next.UTC().Format(time.RFC3339))
		select {
		case <-ctx.Done():
		case t := <-ticker.C:
			next = t.Add(scannerDBInterval)
		}
	}
	oclog.Println("Scanner DB publisher stopped")
	return 0
}

const (
	scannerDBInterval = time.Hour
	// Bundles are 0.6 to 1.5 GB, so a transfer can take minutes. Periodic
	// progress lines show that it is moving and expose a stalled connection.
	scannerDBProgressInterval = 30 * time.Second
	// scannerDBArtifactType marks manifests this publisher owns and may delete.
	scannerDBArtifactType = "application/vnd.stackrox.scanner-db.v1+zip"
)

// scannerDBRegistry is the part of *mirrorclient.MirrorClient the publisher uses.
type scannerDBRegistry interface {
	ManifestHead(ctx context.Context, r ref.Ref) (manifest.Manifest, error)
	ManifestGet(ctx context.Context, r ref.Ref) (manifest.Manifest, error)
	ManifestPut(ctx context.Context, r ref.Ref, m manifest.Manifest) error
	BlobExists(ctx context.Context, r ref.Ref, d descriptor.Descriptor) bool
	BlobPut(ctx context.Context, r ref.Ref, d descriptor.Descriptor, rdr io.Reader) (descriptor.Descriptor, error)
	TagList(ctx context.Context, r ref.Ref) ([]string, error)
	DeleteManifest(ctx context.Context, image string) error
}

var _ scannerDBRegistry = (*mirrorclient.MirrorClient)(nil)

type scannerDBPublisher struct {
	source   string
	dest     ref.Ref
	registry scannerDBRegistry

	// Use the HTTP validator only while the destination still serves the
	// manifest we published. A deleted or overwritten tag needs a fresh copy.
	etag      string
	published digest.Digest

	// Overridable in tests; nil means oclog.Printf and scannerDBProgressInterval.
	logf             func(format string, args ...any)
	progressInterval time.Duration
}

func (p *scannerDBPublisher) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
		return
	}
	oclog.Printf(format, args...)
}

func (p *scannerDBPublisher) sync(ctx context.Context) error {
	p.log("Checking scanner DB for %s", p.dest.CommonName())
	var current digest.Digest
	head, err := p.registry.ManifestHead(ctx, p.dest)
	if err == nil {
		current = head.GetDescriptor().Digest
	} else if !errors.Is(err, errs.ErrNotFound) {
		return fmt.Errorf("read destination tag: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.source, nil)
	if err != nil {
		return err
	}
	if current != "" && current == p.published && p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified && req.Header.Get("If-None-Match") != "" {
		p.log("Scanner DB unchanged upstream (ETag %s), nothing to download", p.etag)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", p.source, resp.Status)
	}

	p.log("Downloading %s (%s)", p.source, formatProgress(-1, resp.ContentLength))
	published, err := p.publish(ctx, resp.Body, path.Base(req.URL.Path), current, resp.ContentLength)
	if err != nil {
		return err
	}
	p.published = published
	p.etag = resp.Header.Get("ETag")
	return nil
}

func (p *scannerDBPublisher) publish(ctx context.Context, body io.Reader, filename string, current digest.Digest, total int64) (digest.Digest, error) {
	file, err := os.CreateTemp("", "scanner-db-*.zip")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	// The worker runs on Linux. Unlink immediately so container restarts
	// cannot leave a partially downloaded database on the pod's emptyDir.
	if err := os.Remove(file.Name()); err != nil {
		return "", err
	}
	hash := digest.SHA256.Digester()
	download := &progressReader{r: body}
	stop := p.reportProgress("Downloading", download, total)
	size, err := io.Copy(io.MultiWriter(file, hash.Hash()), download)
	stop()
	if err != nil {
		return "", fmt.Errorf("download scanner DB after %s: %w", formatProgress(download.n.Load(), total), err)
	}
	p.log("Downloaded %s (%s)", formatProgress(size, -1), hash.Digest())
	archive, err := zip.NewReader(file, size)
	if err != nil {
		return "", fmt.Errorf("invalid scanner DB ZIP: %w", err)
	}
	if len(archive.File) == 0 {
		return "", fmt.Errorf("empty scanner DB ZIP")
	}
	layer := descriptor.Descriptor{
		MediaType: "application/zip", Digest: hash.Digest(), Size: size,
		Annotations: map[string]string{"org.opencontainers.image.title": filename},
	}
	cfg := descriptor.Descriptor{MediaType: mediatype.OCI1Empty, Digest: descriptor.EmptyDigest, Size: int64(len(descriptor.EmptyData))}
	m, err := manifest.New(manifest.WithOrig(v1.Manifest{
		Versioned: v1.ManifestSchemaVersion, MediaType: mediatype.OCI1Manifest,
		ArtifactType: scannerDBArtifactType,
		Config:       cfg, Layers: []descriptor.Descriptor{layer},
	}))
	if err != nil {
		return "", err
	}

	if current == m.GetDescriptor().Digest {
		p.log("Destination already serves this scanner DB, skipping upload")
		return current, nil
	}
	if p.registry.BlobExists(ctx, p.dest, layer) {
		p.log("Scanner DB layer already in the destination repository, publishing the manifest only")
	} else {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		p.log("Uploading scanner DB layer (%s) to %s", formatProgress(size, -1), p.dest.CommonName())
		upload := &progressReader{r: file}
		stop := p.reportProgress("Uploading", upload, size)
		_, err := p.registry.BlobPut(ctx, p.dest, layer, upload)
		stop()
		if err != nil {
			return "", fmt.Errorf("push scanner DB layer: %w", err)
		}
	}
	if !p.registry.BlobExists(ctx, p.dest, cfg) {
		if _, err := p.registry.BlobPut(ctx, p.dest, cfg, bytes.NewReader(descriptor.EmptyData)); err != nil {
			return "", fmt.Errorf("push scanner DB config: %w", err)
		}
	}
	if err := p.registry.ManifestPut(ctx, p.dest, m); err != nil {
		return "", fmt.Errorf("publish scanner DB manifest: %w", err)
	}
	p.log("Published scanner DB manifest %s", m.GetDescriptor().Digest)
	if current != "" {
		p.removeSuperseded(ctx, current)
	}
	return m.GetDescriptor().Digest, nil
}

// removeSuperseded deletes the manifest the tag pointed to before the update.
// Upstream refreshes the bundle about every three hours, so keeping each
// untagged predecessor would retain 0.6 to 1.5 GB per refresh until registry
// garbage collection runs. Only a scanner DB manifest that no other tag
// references is deleted, because deleting by digest removes every tag that
// shares it. Failures are logged: the new bundle is already published.
func (p *scannerDBPublisher) removeSuperseded(ctx context.Context, old digest.Digest) {
	keep := func(format string, args ...any) {
		p.log("Keeping superseded scanner DB manifest %s: %s", old, fmt.Sprintf(format, args...))
	}
	oldRef := p.dest.SetDigest(old.String())
	m, err := p.registry.ManifestGet(ctx, oldRef)
	if err != nil {
		keep("read manifest: %v", err)
		return
	}
	if orig, ok := m.GetOrig().(v1.Manifest); !ok || orig.ArtifactType != scannerDBArtifactType {
		keep("not published by the scanner DB publisher")
		return
	}
	tags, err := p.registry.TagList(ctx, p.dest)
	if err != nil {
		keep("list tags: %v", err)
		return
	}
	for _, tag := range tags {
		if tag == p.dest.Tag {
			continue
		}
		head, err := p.registry.ManifestHead(ctx, p.dest.SetTag(tag))
		if err != nil {
			keep("check tag %s: %v", tag, err)
			return
		}
		if head.GetDescriptor().Digest == old {
			keep("still tagged as %s", tag)
			return
		}
	}
	if err := p.registry.DeleteManifest(ctx, oldRef.CommonName()); err != nil {
		keep("%v; registry garbage collection must reclaim it", err)
		return
	}
	p.log("Deleted superseded scanner DB manifest %s", old)
}

// reportProgress logs the transferred bytes periodically until stop is called.
func (p *scannerDBPublisher) reportProgress(action string, r *progressReader, total int64) (stop func()) {
	every := p.progressInterval
	if every <= 0 {
		every = scannerDBProgressInterval
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				p.log("%s scanner DB: %s", action, formatProgress(r.n.Load(), total))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// progressReader counts the bytes read through it. It deliberately exposes
// only Read and Seek: embedding *os.File would also expose WriteTo, letting
// io.Copy bypass the counter.
type progressReader struct {
	r io.Reader
	n atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n.Add(int64(n))
	return n, err
}

// Seek lets MirrorClient.BlobPut rewind the upload for its HTTPS fallback.
func (p *progressReader) Seek(offset int64, whence int) (int64, error) {
	s, ok := p.r.(io.Seeker)
	if !ok {
		return 0, errors.New("scanner DB reader is not seekable")
	}
	pos, err := s.Seek(offset, whence)
	if err == nil {
		p.n.Store(pos)
	}
	return pos, err
}

// formatProgress renders done and total in binary units; a negative value
// is omitted (unknown total, or no progress to report yet).
func formatProgress(done, total int64) string {
	switch {
	case done < 0 && total < 0:
		return "size unknown"
	case done < 0:
		return formatBytes(total)
	case total <= 0:
		return formatBytes(done)
	}
	return fmt.Sprintf("%s of %s, %d%%", formatBytes(done), formatBytes(total), done*100/total)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
