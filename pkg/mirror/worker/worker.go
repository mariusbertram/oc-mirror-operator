// Package worker implements the worker pod's mirroring loop and its status
// client for the manager's /status and /should-mirror endpoints. Both the
// operator binary's "worker" subcommand (cmd/main.go) and the dedicated
// worker binary (cmd/worker) are thin wrappers around it.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

const (
	// CopyAttempts is how often a worker tries to copy one image before
	// reporting it as failed. The manager does the higher-level retrying.
	CopyAttempts = 2
	// CopyTimeout bounds one copy attempt, so a stalled blob upload cannot
	// hang the worker.
	CopyTimeout = 20 * time.Minute
	// CopyRetryDelay is the pause between two copy attempts.
	CopyRetryDelay = 15 * time.Second
	// VerifyTimeout bounds the digest verification after a copy.
	VerifyTimeout = 2 * time.Minute
	// PruneTimeout bounds deleting the unwanted architectures of an image
	// during an architecture reconcile. It is not part of ImageBudget: a
	// prune runs only after a successful copy (or instead of one), never
	// after the worst case of every copy attempt timing out.
	PruneTimeout = 2 * time.Minute
	// ImageBudget is the worst-case time a worker spends on one image. The
	// manager derives the worker pod's activeDeadlineSeconds from it.
	ImageBudget = CopyAttempts*CopyTimeout + (CopyAttempts-1)*CopyRetryDelay + VerifyTimeout

	// ClientRefreshInterval is the number of images after which the worker
	// builds a fresh registry client, so accumulated auth token scopes do
	// not exceed proxy header limits ("Request Header Too Large").
	ClientRefreshInterval = 20
	// PlanTimeout bounds the blob-sharing analysis that orders a batch.
	PlanTimeout = 10 * time.Minute
)

var log = logf.Log.WithName("worker")

// BatchItem is one image of a worker batch (the MIRROR_BATCH env var holds a
// JSON array of them).
type BatchItem struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
	// Architectures limits a manifest list copy to these CPU architectures
	// (see ImageSet spec.mirror.architectures). Empty means all.
	Architectures []string `json:"architectures,omitempty"`
	// SkipMissingArchitectures copies a manifest list without the child
	// manifests missing at the source (see ImageSet
	// spec.mirror.skipMissingArchitectures).
	SkipMissingArchitectures bool `json:"skipMissingArchitectures,omitempty"`
	// Backfill and PruneRetain make the item an architecture reconcile of an
	// image that is already mirrored, after its ImageSet's architectures
	// changed. Backfill copies the selected children even though the
	// manifest list exists at the destination (a widened selection).
	// PruneRetain, when set, deletes the destination's children whose
	// architecture is not in it (a narrowed selection; only with
	// cleanup-policy=Delete). The manifest list itself never changes.
	Backfill    bool     `json:"backfill,omitempty"`
	PruneRetain []string `json:"pruneRetain,omitempty"`
}

// filter returns the copy filter the item asks for. Copies are always
// recursive: if the destination already holds the manifest list (an earlier,
// possibly sparser mirror of the same image), regclient would otherwise skip
// it and report success while selected children are still missing. For a
// destination without the list it makes no difference.
func (i BatchItem) filter() mirrorclient.CopyFilter {
	return mirrorclient.CopyFilter{
		Architectures:  i.Architectures,
		SkipMissing:    i.SkipMissingArchitectures,
		ForceRecursive: true,
	}
}

// reconcile reports whether the item reconciles an already mirrored image.
func (i BatchItem) reconcile() bool {
	return i.Backfill || len(i.PruneRetain) > 0
}

// ParseBatch decodes a MIRROR_BATCH value.
func ParseBatch(batchJSON string) ([]BatchItem, error) {
	var items []BatchItem
	if err := json.Unmarshal([]byte(batchJSON), &items); err != nil {
		return nil, fmt.Errorf("parse MIRROR_BATCH: %w", err)
	}
	return items, nil
}

// Client is the part of the registry client the worker uses.
// *mirrorclient.MirrorClient implements it.
type Client interface {
	CopyImage(ctx context.Context, src, dest string) (string, error)
	GetDigest(ctx context.Context, image string) (string, error)
}

// BufferedCopier is implemented by clients that can copy an image with large
// blobs buffered to local disk instead of streamed (see
// mirrorclient.MirrorClient.CopyImageBuffered). The worker uses it for the
// last copy attempt, after streaming failed.
type BufferedCopier interface {
	CopyImageBuffered(ctx context.Context, src, dest string) (string, error)
}

// FilteredCopier is implemented by clients that can copy only some child
// manifests of a manifest list (see
// mirrorclient.MirrorClient.CopyImageFiltered).
type FilteredCopier interface {
	CopyImageFiltered(ctx context.Context, src, dest string, f mirrorclient.CopyFilter, buffered bool) (string, []string, error)
}

// ArchitecturePruner is implemented by clients that can delete the child
// manifests of unwanted architectures at the destination (see
// mirrorclient.MirrorClient.PruneArchitectures).
type ArchitecturePruner interface {
	PruneArchitectures(ctx context.Context, dest string, want, retain []string) ([]string, error)
}

// NewMirrorClient builds a registry client for the registry host of
// firstDest, using the Docker config at $DOCKER_CONFIG. With insecure, that
// host is accessed without TLS verification.
func NewMirrorClient(insecure bool, firstDest string) *mirrorclient.MirrorClient {
	var insecureHosts []string
	destHost, _, _ := strings.Cut(firstDest, "/")
	if insecure && destHost != "" {
		insecureHosts = append(insecureHosts, destHost)
	}
	return mirrorclient.NewMirrorClient(insecureHosts, os.Getenv("DOCKER_CONFIG"), destHost)
}

// Worker mirrors images and reports each result to the manager.
type Worker struct {
	// NewClient returns a registry client for the registry of firstDest.
	NewClient func(firstDest string) Client
	// Plan orders a batch for maximal blob reuse.
	Plan func(ctx context.Context, c Client, sources, dests []string) ([]string, []string)
	// Status reports results to the manager.
	Status *StatusClient
	// RetryDelay is the pause between copy attempts.
	RetryDelay time.Duration
}

// New returns a Worker using real registry clients and the manager
// connection described by the environment (see StatusClientFromEnv).
func New(insecure bool) *Worker {
	return &Worker{
		NewClient: func(firstDest string) Client { return NewMirrorClient(insecure, firstDest) },
		Plan:      planWithMirrorClient,
		Status:    StatusClientFromEnv(),
		// Keep the real delay in sync with ImageBudget.
		RetryDelay: CopyRetryDelay,
	}
}

// planWithMirrorClient orders the batch with mirror.PlanMirrorOrder when c
// is a real registry client, and keeps the given order otherwise.
func planWithMirrorClient(ctx context.Context, c Client, sources, dests []string) ([]string, []string) {
	mc, ok := c.(*mirrorclient.MirrorClient)
	if !ok {
		return sources, dests
	}
	return mirror.PlanMirrorOrder(ctx, mc, sources, dests)
}

// RunBatch mirrors every item of a batch, skipping those the manager no
// longer needs, and reports whether any image failed. Individual failures
// are reported through the status API so the manager can apply its per-image
// retry logic; the worker itself still succeeds.
func (w *Worker) RunBatch(ctx context.Context, items []BatchItem) (anyFailed bool) {
	if len(items) == 0 {
		oclog.Println("Empty batch, nothing to do")
		return false
	}

	// A single client is reused for the batch, refreshed periodically.
	c := w.NewClient(items[0].Dest)

	// Images with the most shared blobs go first so that later images find
	// those blobs via anonymous mount (zero-copy).
	sources := make([]string, len(items))
	dests := make([]string, len(items))
	// Plan reorders sources/dests, so items are looked up by dest.
	byDest := make(map[string]BatchItem, len(items))
	for i, item := range items {
		sources[i] = item.Source
		dests[i] = item.Dest
		byDest[item.Dest] = item
	}
	if w.Plan != nil {
		planCtx, planCancel := context.WithTimeout(ctx, PlanTimeout)
		sources, dests = w.Plan(planCtx, c, sources, dests)
		planCancel()
	}

	for i := range sources {
		// The user may have shrunk the ImageSet (operator removed, version
		// range narrowed) since this batch was dispatched, or a parallel
		// worker may have mirrored the image after a re-collection.
		item, ok := byDest[dests[i]]
		if !ok {
			item = BatchItem{Source: sources[i], Dest: dests[i]}
		}
		if item.reconcile() {
			if !w.Status.ShouldReconcile(ctx, dests[i]) {
				oclog.Printf("Skipping architecture reconcile of %s: no longer mirrored or required\n", dests[i])
				continue
			}
		} else if !w.Status.ShouldMirror(ctx, dests[i]) {
			oclog.Printf("Skipping %s: no longer required by any ImageSet\n", dests[i])
			continue
		}
		if i > 0 && i%ClientRefreshInterval == 0 {
			c = w.NewClient(dests[i])
		}
		if mirror.IsChartSource(sources[i]) {
			if !w.MirrorChartOne(ctx, c, sources[i], dests[i]) {
				anyFailed = true
			}
			continue
		}
		if !w.mirrorOne(ctx, c, item) {
			anyFailed = true
		}
	}
	if anyFailed {
		oclog.Println("Batch completed with errors (see above)")
	}
	return anyFailed
}

// MirrorChartOne mirrors a single Helm chart archive (a helm:// source, see
// mirror.HelmChartSource) into the target registry as a Helm OCI artifact,
// with the same retry and status reporting as MirrorOne. It returns true on
// success. Unlike images, a chart is re-downloaded from its repository on
// every attempt — the archive is small (KB to a few MB) and not stored in a
// registry the worker could copy from.
func (w *Worker) MirrorChartOne(ctx context.Context, c Client, src, dest string) bool {
	oclog.Printf("Starting chart mirror: %s -> %s\n", src, dest)
	mc, ok := c.(*mirrorclient.MirrorClient)
	if !ok {
		err := fmt.Errorf("chart mirror requires a real registry client")
		log.Error(err, "cannot mirror helm chart")
		w.Status.Report(ctx, dest, "", err.Error())
		return false
	}

	var lastErr error
	for attempt := 1; attempt <= CopyAttempts; attempt++ {
		if attempt > 1 {
			oclog.Printf("Retry attempt %d/%d after %s...\n", attempt, CopyAttempts, w.RetryDelay)
			time.Sleep(w.RetryDelay)
		}
		chartCtx, cancel := context.WithTimeout(ctx, ChartMirrorTimeout)
		_, lastErr = MirrorChart(chartCtx, mc, src, dest)
		cancel()
		if lastErr == nil {
			break
		}
		oclog.Printf("Attempt %d failed: %v\n", attempt, lastErr)
	}
	if lastErr != nil {
		oclog.Printf("ERROR: failed to mirror chart %s: %v", src, lastErr)
		log.Error(lastErr, "failed to mirror helm chart")
		w.Status.Report(ctx, dest, "", lastErr.Error())
		return false
	}

	verifyCtx, verifyCancel := context.WithTimeout(ctx, VerifyTimeout)
	digest, err := mc.GetDigest(verifyCtx, dest)
	verifyCancel()
	if err != nil {
		oclog.Printf("ERROR: failed to verify digest for chart %s: %v", src, err)
		log.Error(err, "failed to verify mirrored chart digest")
		w.Status.Report(ctx, dest, "", err.Error())
		return false
	}

	oclog.Printf("Successfully mirrored %s -> %s (digest: %s)\n", src, dest, digest)
	log.Info("successfully mirrored helm chart", "src", src, "dest", dest, "digest", digest)
	w.Status.Report(ctx, dest, digest, "")
	return true
}

// MirrorOne mirrors src→dest with up to CopyAttempts attempts, verifies the
// digest at the destination, and reports the result to the manager. It
// returns true on success. Blobs are streamed; when c is a BufferedCopier,
// the last attempt buffers large blobs to disk instead.
func (w *Worker) MirrorOne(ctx context.Context, c Client, src, dest string) bool {
	return w.mirrorOne(ctx, c, BatchItem{Source: src, Dest: dest})
}

// mirrorOne is MirrorOne for a batch item: only the child manifests of a
// manifest list that the item's filter selects are copied (when c supports
// it, see FilteredCopier), and an architecture reconcile backfills and/or
// prunes an already mirrored image (see BatchItem.Backfill/PruneRetain). The
// report carries the item's architecture selection so the manager can record
// what the image was mirrored with.
func (w *Worker) mirrorOne(ctx context.Context, c Client, item BatchItem) bool {
	src, dest := item.Source, item.Dest
	reconcile := item.reconcile()
	report := StatusRequest{Destination: dest, Reconcile: reconcile, Architectures: item.Architectures}
	fail := func(msg string, err error) bool {
		oclog.Printf("ERROR: %s %s: %v", msg, src, err)
		log.Error(err, msg, "src", src, "dest", dest)
		report.Error = err.Error()
		w.Status.ReportResult(ctx, report)
		return false
	}

	effectiveDest := dest
	if !reconcile || item.Backfill {
		if reconcile {
			oclog.Printf("Backfilling architectures %v: %s -> %s\n", item.Architectures, src, dest)
		} else {
			oclog.Printf("Starting mirror: %s -> %s\n", src, dest)
		}
		var err error
		effectiveDest, err = w.copyWithRetries(ctx, c, src, dest, item.filter())
		if err != nil {
			return fail("failed to mirror", err)
		}
	}
	if len(item.PruneRetain) > 0 {
		p, ok := c.(ArchitecturePruner)
		if !ok {
			return fail("failed to prune architectures of", fmt.Errorf("registry client cannot prune architectures"))
		}
		// The plan was made at dispatch; the policy or the architectures
		// the repository needs may have changed since.
		if !w.Status.ConfirmPrune(ctx, dest, item.PruneRetain) {
			return fail("not pruning architectures of", fmt.Errorf("manager did not confirm the prune plan (keep %v); it is retried with the current plan", item.PruneRetain))
		}
		pruneCtx, cancel := context.WithTimeout(ctx, PruneTimeout)
		deleted, err := p.PruneArchitectures(pruneCtx, effectiveDest, item.Architectures, item.PruneRetain)
		cancel()
		if len(deleted) > 0 {
			oclog.Printf("Deleted child manifests of %s outside %v: %s\n", effectiveDest, item.PruneRetain, strings.Join(deleted, ", "))
		}
		if err != nil {
			return fail("failed to prune architectures of", err)
		}
		report.PruneRetain = item.PruneRetain
	}

	oclog.Printf("Copy complete, verifying digest at %s\n", effectiveDest)
	verifyCtx, verifyCancel := context.WithTimeout(ctx, VerifyTimeout)
	digest, err := c.GetDigest(verifyCtx, effectiveDest)
	verifyCancel()
	if err != nil {
		return fail("failed to verify digest for", err)
	}

	oclog.Printf("Successfully mirrored %s -> %s (digest: %s)\n", src, dest, digest)
	log.Info("successfully mirrored image", "src", src, "dest", dest, "digest", digest)
	report.Digest = digest
	w.Status.ReportResult(ctx, report)
	return true
}

// copyWithRetries copies src→dest with up to CopyAttempts attempts and
// returns the effective destination. Blobs are streamed; when c is a
// BufferedCopier, the last attempt buffers large blobs to disk instead. A
// non-zero f is applied when c is a FilteredCopier.
func (w *Worker) copyWithRetries(ctx context.Context, c Client, src, dest string, f mirrorclient.CopyFilter) (string, error) {
	var effectiveDest string
	var lastErr error
	for attempt := 1; attempt <= CopyAttempts; attempt++ {
		copyFn := c.CopyImage
		buffered := false
		if attempt > 1 {
			oclog.Printf("Retry attempt %d/%d after %s...\n", attempt, CopyAttempts, w.RetryDelay)
			time.Sleep(w.RetryDelay)
			if bc, ok := c.(BufferedCopier); ok && attempt == CopyAttempts {
				oclog.Println("Streaming copy failed, retrying with large blobs buffered to disk")
				copyFn = bc.CopyImageBuffered
				buffered = true
			}
		}
		if fc, ok := c.(FilteredCopier); ok && !f.IsZero() {
			copyFn = func(ctx context.Context, src, dest string) (string, error) {
				eff, missing, err := fc.CopyImageFiltered(ctx, src, dest, f, buffered)
				if len(missing) > 0 {
					oclog.Printf("Skipping child manifests missing at the source of %s: %s\n", src, strings.Join(missing, ", "))
				}
				return eff, err
			}
		}
		copyCtx, cancel := context.WithTimeout(ctx, CopyTimeout)
		effectiveDest, lastErr = copyFn(copyCtx, src, dest)
		cancel()
		if lastErr == nil {
			return effectiveDest, nil
		}
		oclog.Printf("Attempt %d failed: %v\n", attempt, lastErr)
	}
	return "", lastErr
}
