/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mirrorv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))

	if len(os.Args) > 1 && os.Args[1] == "cleanup" {
		runCleanup()
		return
	}

	// Default: runWorker
	runWorker()
}

// runWorker mirrors image batches from MIRROR_BATCH environment variable.
// Can also be called with legacy --src/--dest flags for single-image mode.
func runWorker() {
	var insecure bool
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	fs.BoolVar(&insecure, "insecure", false, "Allow insecure registry")
	// --src / --dest kept for backward compatibility
	var src, dest string
	fs.StringVar(&src, "src", "", "Source image (legacy single-image mode)")
	fs.StringVar(&dest, "dest", "", "Destination image (legacy single-image mode)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "flag parse error:", err)
		os.Exit(1)
	}

	// Batch mode: process all images in the JSON-encoded MIRROR_BATCH env var.
	if batchJSON := os.Getenv("MIRROR_BATCH"); batchJSON != "" {
		runWorkerBatch(insecure, batchJSON)
		return
	}

	// Legacy single-image mode (used only when called without MIRROR_BATCH).
	if src == "" || dest == "" {
		fmt.Fprintln(os.Stderr, "ERROR: MIRROR_BATCH env var or --src/--dest flags are required")
		os.Exit(1)
	}
	c := buildMirrorClient(insecure, dest)
	if !mirrorOneImage(c, src, dest) {
		os.Exit(1)
	}
}

type BatchItem struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

func runWorkerBatch(insecure bool, batchJSON string) {
	var items []BatchItem
	if err := json.Unmarshal([]byte(batchJSON), &items); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to parse MIRROR_BATCH: %v\n", err)
		os.Exit(1)
	}
	if len(items) == 0 {
		fmt.Println("Empty batch, nothing to do")
		return
	}

	// Build a single client reused for all images in the batch.
	c := buildMirrorClient(insecure, items[0].Dest)

	// Compute optimal mirror order: images with the most shared blobs go first
	// so that subsequent images find those blobs via anonymous mount (zero-copy).
	sources := make([]string, len(items))
	dests := make([]string, len(items))
	for i, item := range items {
		sources[i] = item.Source
		dests[i] = item.Dest
	}
	planCtx, planCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	sources, dests = mirror.PlanMirrorOrder(planCtx, c, sources, dests)
	planCancel()

	anyFailed := false
	for i := range sources {
		// Ask the manager whether this destination is still required. The
		// user may have shrunk the ImageSet (operator removed, version
		// range narrowed) since this batch was dispatched. If the image is
		// no longer needed (or has already been mirrored by a parallel
		// worker after a re-collection), skip it instead of wasting bandwidth
		// and registry storage.
		if !shouldMirror(dests[i]) {
			fmt.Printf("Skipping %s: no longer required by any ImageSet\n", dests[i])
			continue
		}
		// Refresh the mirror client every 20 images to prevent auth token
		// accumulation that causes "Request Header Too Large" (nginx 8KB limit).
		if i > 0 && i%20 == 0 {
			c = buildMirrorClient(insecure, dests[i])
		}
		if !mirrorOneImage(c, sources[i], dests[i]) {
			anyFailed = true
		}
	}
	// Exit 0 even if some images failed; individual failures are reported via
	// the status API so the manager can apply per-image retry logic.
	if anyFailed {
		fmt.Println("Batch completed with errors (see above)")
	}
}

func buildMirrorClient(insecure bool, firstDest string) *mirrorclient.MirrorClient {
	var insecureHosts []string
	destHost := ""
	if parts := strings.Split(firstDest, "/"); len(parts) > 0 {
		destHost = parts[0]
		if insecure {
			insecureHosts = append(insecureHosts, destHost)
		}
	}
	return mirrorclient.NewMirrorClient(insecureHosts, os.Getenv("DOCKER_CONFIG"), destHost)
}

// mirrorOneImage mirrors src→dest with a single retry for transient errors and
// reports the result to the manager via the status API. Returns true on success.
// The manager handles higher-level retry orchestration (Failed→Pending requeue).
func mirrorOneImage(c *mirrorclient.MirrorClient, src, dest string) bool {
	fmt.Printf("Starting mirror: %s -> %s\n", src, dest)

	// Per-image timeout prevents indefinite hangs on stalled blob uploads.
	const imageTimeout = 20 * time.Minute

	var effectiveDest string
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			fmt.Printf("Retry attempt 2/2 after 15s...\n")
			time.Sleep(15 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), imageTimeout)
		effectiveDest, lastErr = c.CopyImage(ctx, src, dest)
		cancel()
		if lastErr == nil {
			break
		}
		fmt.Printf("Attempt %d failed: %v\n", attempt, lastErr)
	}
	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to mirror %s: %v\n", src, lastErr)
		setupLog.Error(lastErr, "failed to mirror image")
		reportStatus(dest, "", lastErr.Error())
		return false
	}

	fmt.Printf("Copy complete, verifying digest at %s\n", effectiveDest)
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	digest, err := c.GetDigest(verifyCtx, effectiveDest)
	verifyCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to verify digest for %s: %v\n", src, err)
		setupLog.Error(err, "failed to verify mirrored image digest")
		reportStatus(dest, "", err.Error())
		return false
	}

	fmt.Printf("Successfully mirrored %s -> %s (digest: %s)\n", src, dest, digest)
	setupLog.Info("successfully mirrored image", "src", src, "dest", dest, "digest", digest)
	reportStatus(dest, digest, "")
	return true
}

type WorkerStatusRequest struct {
	PodName     string `json:"podName"`
	Destination string `json:"destination"`
	Digest      string `json:"digest"`
	Error       string `json:"error,omitempty"`
}

func reportStatus(dest, digest, errMsg string) {
	managerURL := os.Getenv("MANAGER_URL")
	podName := os.Getenv("POD_NAME")
	if managerURL == "" || podName == "" {
		return
	}

	req := WorkerStatusRequest{
		PodName:     podName,
		Destination: dest,
		Digest:      digest,
		Error:       errMsg,
	}

	body, err := json.Marshal(req)
	if err != nil {
		fmt.Printf("Failed to marshal status request: %v\n", err)
		return
	}

	// Retry the callback up to 3 times with a 2-second delay so transient
	// network blips or manager lock contention don't permanently lose state.
	// The manager's handler is idempotent (same dest + state → no side effects).
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(2 * time.Second)
		}
		httpReq, reqErr := http.NewRequestWithContext(context.Background(), "POST", managerURL+"/status", bytes.NewBuffer(body))
		if reqErr != nil {
			fmt.Printf("Failed to build status request: %v\n", reqErr)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("WORKER_TOKEN"))
		httpClient := &http.Client{Timeout: 10 * time.Second}
		resp, doErr := httpClient.Do(httpReq)
		if doErr != nil {
			fmt.Printf("Status callback attempt %d/%d failed: %v\n", attempt, 3, doErr)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		fmt.Printf("Status callback attempt %d/%d: HTTP %d\n", attempt, 3, resp.StatusCode)
	}
	fmt.Printf("Failed to report status to manager after 3 attempts for %s\n", dest)
}

// shouldMirror queries the manager whether `dest` is still required.
// Returns true on 200 OK, false on 410 Gone. On any error (network failure,
// missing env vars, manager unreachable) it returns true so the worker fails
// safe: an outdated mirror is preferable to skipping a still-required image.
func shouldMirror(dest string) bool {
	managerURL := os.Getenv("MANAGER_URL")
	if managerURL == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "GET",
		managerURL+"/should-mirror?dest="+url.QueryEscape(dest), nil)
	if err != nil {
		return true
	}
	httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("WORKER_TOKEN"))
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		fmt.Printf("Failed to query /should-mirror for %s: %v (proceeding)\n", dest, err)
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode != http.StatusGone
}

// runCleanup deletes all images for a removed ImageSet from the target registry
// and removes the associated image state ConfigMap.
func runCleanup() {
	var imageSetName, namespace, registry, configMapName string
	var insecure bool
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	fs.StringVar(&imageSetName, "imageset", "", "Name of the ImageSet to clean up")
	fs.StringVar(&namespace, "namespace", "", "Namespace of the ImageSet")
	fs.StringVar(&registry, "registry", "", "Target registry URL")
	fs.BoolVar(&insecure, "insecure", false, "Allow insecure registry")
	fs.StringVar(&configMapName, "configmap", "", "Override ConfigMap name (default: derived from --imageset)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "flag parse error:", err)
		os.Exit(1)
	}

	if namespace == "" || registry == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --namespace and --registry are required")
		os.Exit(1)
	}
	if imageSetName == "" && configMapName == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --imageset or --configmap is required")
		os.Exit(1)
	}
	// Derive ConfigMap name from imageset if not explicitly set.
	if configMapName == "" {
		configMapName = imagestate.ConfigMapName(imageSetName) //nolint:staticcheck // migration pending
	}

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Load the image state from the specified ConfigMap.
	state, err := imagestate.LoadByConfigMapName(ctx, c, namespace, configMapName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to load image state from %s: %v\n", configMapName, err)
		os.Exit(1)
	}

	if len(state) == 0 {
		fmt.Printf("No images found in %s — nothing to clean up\n", configMapName)
		deleteConfigMapByName(ctx, c, namespace, configMapName)
		os.Exit(0)
	}

	fmt.Printf("Cleaning up %d images from %s (registry: %s)\n", len(state), configMapName, registry)

	// Build a registry client for deletion.
	mc := buildMirrorClient(insecure, registry)

	var deleted, skipped, failed int
	// Fresh client every 20 deletes to avoid auth token overflow.
	const refreshInterval = 20
	count := 0

	for dest, entry := range state {
		if entry.State != "Mirrored" {
			skipped++
			continue
		}

		if count > 0 && count%refreshInterval == 0 {
			mc = buildMirrorClient(insecure, registry)
		}
		count++

		func() {
			delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := mc.DeleteManifest(delCtx, dest); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: failed to delete %s: %v\n", dest, err)
				failed++
			} else {
				fmt.Printf("Deleted: %s\n", dest)
				deleted++
			}
		}()
	}

	fmt.Printf("Cleanup complete: %d deleted, %d skipped (not mirrored), %d failed\n", deleted, skipped, failed)

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d images could not be deleted\n", failed)
		os.Exit(1)
	}

	// All images deleted successfully — remove the ConfigMap.
	deleteConfigMapByName(ctx, c, namespace, configMapName)
}

func deleteConfigMapByName(ctx context.Context, c client.Client, namespace, cmName string) {
	cm := &corev1.ConfigMap{}
	cm.Name = cmName
	cm.Namespace = namespace
	if err := c.Delete(ctx, cm); err != nil {
		if !k8serrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "WARN: failed to delete ConfigMap %s: %v\n", cmName, err)
		}
	} else {
		fmt.Printf("Deleted ConfigMap %s\n", cmName)
	}
}
