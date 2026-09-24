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
	for i, item := range items {
		sources[i] = item.Source
		dests[i] = item.Dest
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
		if !w.Status.ShouldMirror(ctx, dests[i]) {
			oclog.Printf("Skipping %s: no longer required by any ImageSet\n", dests[i])
			continue
		}
		if i > 0 && i%ClientRefreshInterval == 0 {
			c = w.NewClient(dests[i])
		}
		if !w.MirrorOne(ctx, c, sources[i], dests[i]) {
			anyFailed = true
		}
	}
	if anyFailed {
		oclog.Println("Batch completed with errors (see above)")
	}
	return anyFailed
}

// MirrorOne mirrors src→dest with up to CopyAttempts attempts, verifies the
// digest at the destination, and reports the result to the manager. It
// returns true on success.
func (w *Worker) MirrorOne(ctx context.Context, c Client, src, dest string) bool {
	oclog.Printf("Starting mirror: %s -> %s\n", src, dest)

	var effectiveDest string
	var lastErr error
	for attempt := 1; attempt <= CopyAttempts; attempt++ {
		if attempt > 1 {
			oclog.Printf("Retry attempt %d/%d after %s...\n", attempt, CopyAttempts, w.RetryDelay)
			time.Sleep(w.RetryDelay)
		}
		copyCtx, cancel := context.WithTimeout(ctx, CopyTimeout)
		effectiveDest, lastErr = c.CopyImage(copyCtx, src, dest)
		cancel()
		if lastErr == nil {
			break
		}
		oclog.Printf("Attempt %d failed: %v\n", attempt, lastErr)
	}
	if lastErr != nil {
		oclog.Printf("ERROR: failed to mirror %s: %v", src, lastErr)
		log.Error(lastErr, "failed to mirror image")
		w.Status.Report(ctx, dest, "", lastErr.Error())
		return false
	}

	oclog.Printf("Copy complete, verifying digest at %s\n", effectiveDest)
	verifyCtx, verifyCancel := context.WithTimeout(ctx, VerifyTimeout)
	digest, err := c.GetDigest(verifyCtx, effectiveDest)
	verifyCancel()
	if err != nil {
		oclog.Printf("ERROR: failed to verify digest for %s: %v", src, err)
		log.Error(err, "failed to verify mirrored image digest")
		w.Status.Report(ctx, dest, "", err.Error())
		return false
	}

	oclog.Printf("Successfully mirrored %s -> %s (digest: %s)\n", src, dest, digest)
	log.Info("successfully mirrored image", "src", src, "dest", dest, "digest", digest)
	w.Status.Report(ctx, dest, digest, "")
	return true
}
