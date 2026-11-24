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

// ScannerDBMain runs in the optional worker container of the manager pod.
// The deployment's Recreate strategy keeps one publisher per target.
func ScannerDBMain(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("acs-scanner-db", flag.ContinueOnError)
	registry := fs.String("registry", "", "Target registry, optionally with a path prefix")
	version := fs.String("version", "", "Published RHACS bundle version (empty for legacy Scanner)")
	insecure := fs.Bool("insecure", false, "Allow insecure target registry")
	if err := fs.Parse(args); err != nil || *registry == "" {
		return 1
	}

	tag, filename := "latest", "scanner-vuln-updates.zip"
	if *version != "" {
		tag, filename = *version, fmt.Sprintf("%[1]s/scanner-vulns-%[1]s.zip", *version)
	}
	dest, err := ref.New(strings.TrimSuffix(*registry, "/") + "/rhacs/scanner-db:" + tag)
	if err != nil {
		oclog.Printf("Invalid scanner DB destination: %v", err)
		return 1
	}
	pub := scannerDBPublisher{
		source: "https://install.stackrox.io/scanner/" + filename, dest: dest,
		registry: NewMirrorClient(*insecure, dest.CommonName()),
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for ctx.Err() == nil {
		syncCtx, cancel := context.WithTimeout(ctx, time.Hour)
		err := pub.sync(syncCtx)
		cancel()
		if err != nil {
			oclog.Printf("Scanner DB sync to %s failed: %v", dest.CommonName(), err)
		} else {
			oclog.Printf("Scanner DB is current at %s@%s", dest.CommonName(), pub.published)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return 0
}

type scannerDBPublisher struct {
	source   string
	dest     ref.Ref
	registry *mirrorclient.MirrorClient

	// Use the HTTP validator only while the destination still serves the
	// manifest we published. A deleted or overwritten tag needs a fresh copy.
	etag      string
	published digest.Digest
}

func (p *scannerDBPublisher) sync(ctx context.Context) error {
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
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", p.source, resp.Status)
	}

	published, err := p.publish(ctx, resp.Body, path.Base(req.URL.Path), current)
	if err != nil {
		return err
	}
	p.published = published
	p.etag = resp.Header.Get("ETag")
	return nil
}

func (p *scannerDBPublisher) publish(ctx context.Context, body io.Reader, filename string, current digest.Digest) (digest.Digest, error) {
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
	size, err := io.Copy(io.MultiWriter(file, hash.Hash()), body)
	if err != nil {
		return "", fmt.Errorf("download scanner DB: %w", err)
	}
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
		ArtifactType: "application/vnd.stackrox.scanner-db.v1+zip",
		Config:       cfg, Layers: []descriptor.Descriptor{layer},
	}))
	if err != nil {
		return "", err
	}

	if current != m.GetDescriptor().Digest {
		if !p.registry.BlobExists(ctx, p.dest, layer) {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return "", err
			}
			if _, err := p.registry.BlobPut(ctx, p.dest, layer, file); err != nil {
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
	}
	return m.GetDescriptor().Digest, nil
}
