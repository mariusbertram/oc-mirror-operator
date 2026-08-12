package manager

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	ocmetrics "github.com/mariusbertram/oc-mirror-operator/pkg/metrics"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/cosign"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
	"github.com/mariusbertram/oc-mirror-operator/pkg/resourceapi"
	"github.com/regclient/regclient/types/errs"
)

// Image entry state constants used throughout the manager.
const (
	stateMirrored = "Mirrored"
	statePending  = "Pending"
	stateFailed   = "Failed"

	conditionReady = "Ready"

	// maxFailedImageDetails caps the number of FailedImageDetail entries
	// written to ImageSet.status to bound the overall status object size.
	// When more images are permanently failed, a summary is appended to the
	// Ready condition message.
	maxFailedImageDetails = 20
)

type WorkerStatusRequest struct {
	PodName     string `json:"podName"`
	Destination string `json:"destination"`
	Digest      string `json:"digest"`
	Error       string `json:"error,omitempty"`
}

// BatchItem describes a single image to be mirrored within a worker batch.
type BatchItem struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

type MirrorManager struct {
	Client         client.Client
	Clientset      kubernetes.Interface
	TargetName     string
	Namespace      string
	Scheme         *runtime.Scheme
	Image          string
	mirrorClient   *mirrorclient.MirrorClient
	authConfigPath string // path to Docker config for creating fresh clients
	clientCache    *mirrorclient.ClientCache

	workerToken string

	// urgentFlush is signalled (non-blocking) whenever a worker completion
	// arrives so the reconcile loop runs immediately instead of waiting for
	// the 30-second ticker.
	urgentFlush chan struct{}

	// State in memory — protected by mu
	mu         sync.RWMutex
	inProgress map[string]string     // dest → podName
	mirrored   map[string]bool       // dest → true once successfully mirrored
	imageState imagestate.ImageState // dest → entry, merged across all ImageSets on this target
	// owners tracks which ImageSet(s) each destination in imageState currently
	// belongs to. It is the in-memory analogue of the on-disk partitioning
	// (each ImageSet's own state ConfigMap + the shared-image index) — kept
	// out-of-band rather than on ImageEntry so imageState can stay a single
	// dest-keyed map for O(1) worker status/should-mirror lookups regardless
	// of how many ImageSets reference a destination.
	owners         map[string][]string
	lastDriftCheck time.Time // last time a drift-check sweep was started
	stateDirty     bool      // true when imageState/owners has unsaved changes
	statusDirty    bool      // true when ImageSet.status needs a Kubernetes write

	// driftSweepRunning is true while a background drift-check sweep (see
	// startDriftSweepLocked) is in flight, so reconcile() doesn't launch a
	// second overlapping one. The sweep itself runs outside reconcile()'s
	// call stack — reconcile() only ever holds m.mu for as long as it takes
	// to snapshot which destinations to check or apply one result, never for
	// the CheckExist network calls themselves, so it stays responsive
	// (dispatching new worker batches, handling status callbacks) for the
	// entire, potentially hours-long, duration of a large sweep.
	driftSweepRunning bool
}

func New(targetName, namespace string, scheme *runtime.Scheme) (*MirrorManager, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	image := os.Getenv("WORKER_IMAGE")
	if image == "" {
		// fallback for backward compatibility
		image = os.Getenv("OPERATOR_IMAGE")
	}
	if image == "" {
		return nil, fmt.Errorf("WORKER_IMAGE environment variable is required but not set")
	}

	authConfigPath := os.Getenv("DOCKER_CONFIG")
	return NewWithClients(c, cs, targetName, namespace, image, authConfigPath, scheme), nil
}

func NewWithClients(c client.Client, cs kubernetes.Interface, targetName, namespace, image, authConfigPath string, scheme *runtime.Scheme) *MirrorManager {
	mc := mirrorclient.NewMirrorClient(nil, authConfigPath)

	return &MirrorManager{
		Client:         c,
		Clientset:      cs,
		TargetName:     targetName,
		Namespace:      namespace,
		Scheme:         scheme,
		Image:          image,
		mirrorClient:   mc,
		authConfigPath: authConfigPath,
		clientCache:    mirrorclient.NewClientCache(),
		urgentFlush:    make(chan struct{}, 1),
		// workerToken is populated lazily by ensureWorkerTokenSecret() in Run().
		inProgress: make(map[string]string),
		mirrored:   make(map[string]bool),
		imageState: make(imagestate.ImageState),
		owners:     make(map[string][]string),
	}
}

// workerTokenSecretName returns the Secret name used to persist the worker
// bearer token across manager restarts.
func (m *MirrorManager) workerTokenSecretName() string {
	return m.TargetName + "-worker-token"
}

// ensureWorkerTokenSecret loads the worker bearer token from a dedicated Secret
// or creates the Secret with a freshly generated 32-byte token if it does not
// yet exist. Persisting the token in a Secret avoids leaking it via plain
// `env.value` in worker pod specs (which any user with `pods/get` could read)
// and lets worker pods that survive a manager restart keep authenticating with
// the same token.
func (m *MirrorManager) ensureWorkerTokenSecret(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	name := m.workerTokenSecretName()

	existing := &corev1.Secret{}
	getErr := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, existing)
	if getErr == nil {
		tok, ok := existing.Data["token"]
		if !ok || len(tok) == 0 {
			return fmt.Errorf("worker token secret %s exists but has no 'token' key", name)
		}
		m.workerToken = string(tok)
		return nil
	}
	if !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("get worker token secret: %w", getErr)
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generate worker token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}
	if mt != nil {
		if err := controllerutil.SetControllerReference(mt, sec, m.Scheme); err != nil {
			return fmt.Errorf("set owner on worker token secret: %w", err)
		}
	}
	if err := m.Client.Create(ctx, sec); err != nil {
		return fmt.Errorf("create worker token secret: %w", err)
	}
	m.workerToken = token
	return nil
}

func (m *MirrorManager) Run(ctx context.Context) error {
	oclog.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

	// Load (or create) the worker bearer token from its Secret. We need the
	// MirrorTarget object as the OwnerReference so the Secret is GC'd when the
	// MirrorTarget is deleted.
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt); err != nil {
		return fmt.Errorf("load MirrorTarget for token bootstrap: %w", err)
	}
	if err := m.ensureWorkerTokenSecret(ctx, mt); err != nil {
		return fmt.Errorf("worker token bootstrap: %w", err)
	}

	// Rebuild in-progress state from any worker pods that survived a manager restart.
	if err := m.syncInProgressFromPods(ctx); err != nil {
		oclog.Printf("Warning: could not sync in-progress state from pods: %v\n", err)
	}

	// Start Status API Server (internal, port 8080)
	// Workers POST status updates here and query should-mirror decisions
	go m.runStatusAPI(ctx)

	// Start Prometheus metrics endpoint (port 9090)
	go m.runMetricsServer(ctx)

	// Start Resource API Server (external, port 8000)
	// UI/CLI queries target status, catalogs, resources (IDMS/ITMS)
	go func() {
		srv := resourceapi.NewServer(m.Client, m.Namespace)
		oclog.Println("Starting Resource API Server on :8000")
		srv.Run(ctx)
	}()

	// Run reconcile once immediately on startup, then every 30s.
	if err := m.reconcile(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error reconciling: %v\n", err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.urgentFlush:
			// A worker completion arrived — flush state immediately without
			// waiting for the full 30-second tick. Drain the ticker so the
			// next tick resets from now rather than firing immediately after.
			select {
			case <-ticker.C:
			default:
			}
			ticker.Reset(30 * time.Second)
			if err := m.reconcile(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Error reconciling: %v\n", err)
			}
		case <-ticker.C:
			if err := m.reconcile(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Error reconciling: %v\n", err)
			}
		}
	}
}

// syncInProgressFromPods rebuilds m.inProgress from existing worker pods so that
// a manager restart does not re-dispatch images that are already being mirrored.
// It also deletes any completed/failed worker pods left over from a previous run.
func (m *MirrorManager) syncInProgressFromPods(ctx context.Context) error {
	pods, err := m.Clientset.CoreV1().Pods(m.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=oc-mirror-worker,mirrortarget=%s", m.TargetName),
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			oclog.Printf("Cleaning up finished worker pod %s (%s)\n", pod.Name, pod.Status.Phase)
			_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			continue
		}
		// New multi-dest annotation (batch mode)
		if destsJSON, ok := pod.Annotations["mirror.openshift.io/destinations"]; ok && destsJSON != "" {
			var dests []string
			if json.Unmarshal([]byte(destsJSON), &dests) == nil {
				for _, dest := range dests {
					m.inProgress[dest] = pod.Name
					oclog.Printf("Recovered in-progress worker %s for %s\n", pod.Name, dest)
				}
				continue
			}
		}
		// Backward compat: legacy single-dest annotation
		if dest, ok := pod.Annotations["mirror.openshift.io/destination"]; ok && dest != "" {
			m.inProgress[dest] = pod.Name
			oclog.Printf("Recovered in-progress worker %s for %s\n", pod.Name, dest)
		}
	}
	return nil
}

func (m *MirrorManager) runMetricsServer(ctx context.Context) {
	server := &http.Server{
		Addr:              ":9090",
		Handler:           ocmetrics.NewManagerMetricsHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		oclog.Println("Manager metrics server started on :9090")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			oclog.Printf("Metrics server failed: %v\n", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func (m *MirrorManager) runStatusAPI(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", m.handleStatusUpdate)
	mux.HandleFunc("/should-mirror", m.handleShouldMirror)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			oclog.Printf("Status API server failed: %v\n", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func (m *MirrorManager) handleStatusUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	expected := "Bearer " + m.workerToken
	// Constant-time comparison prevents timing side-channels that could leak
	// the token byte-by-byte to an attacker probing the status endpoint.
	if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	defer func() { _ = r.Body.Close() }()

	var req WorkerStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	oclog.Printf("Received status update from %s for %s\n", req.PodName, req.Destination)

	imageset := ""
	if names := m.owners[req.Destination]; len(names) > 0 {
		imageset = names[0]
	}

	if req.Error != "" {
		m.setImageStateLocked(req.Destination, stateFailed, req.Error)
		ocmetrics.ManagerImagesFailedTotal.WithLabelValues(m.TargetName, imageset).Inc()
	} else {
		m.mirrored[req.Destination] = true
		m.setImageStateLocked(req.Destination, stateMirrored, "")
		// Record the digest the worker actually mirrored as the drift-check
		// baseline for tag-referenced additional images (see
		// additionalImageDriftedLocked) — free, since the worker already
		// resolves it to verify the copy. setImageStateLocked's idempotency
		// check can early-return without this running again on a duplicate
		// callback, but SourceDigest doesn't change between duplicates either.
		if entry, ok := m.imageState[req.Destination]; ok && entry.Origin == imagestate.OriginAdditional && req.Digest != "" {
			entry.SourceDigest = req.Digest
			m.stateDirty = true
		}
		ocmetrics.ManagerImagesMirroredTotal.WithLabelValues(m.TargetName, imageset).Inc()
	}

	// Remove from in-progress tracking. The pod itself is cleaned up by
	// cleanupFinishedWorkers() once it reaches Succeeded/Failed, so that
	// other batch items in the same pod can continue reporting.
	delete(m.inProgress, req.Destination)
	m.statusDirty = true

	// Signal the reconcile loop to run immediately instead of waiting for
	// the 30-second ticker. Non-blocking send: if a signal is already queued
	// (channel capacity = 1) we skip — the pending reconcile will pick this up.
	select {
	case m.urgentFlush <- struct{}{}:
	default:
	}

	w.WriteHeader(http.StatusOK)
}

// handleShouldMirror lets a worker check, just before mirroring an image,
// whether the image is still required by any ImageSet on this MirrorTarget.
// This prevents wasting work when the user shrinks an ImageSet (removed
// operator, narrowed version range) while a worker batch is still in flight.
//
// Responses:
//
//	200 OK    — image is still pending or failed (worker should mirror it)
//	410 Gone  — image is already Mirrored or no longer in any state
//	            (worker MUST skip it)
//	401 Unauthorized — bad/missing Bearer token
//	400 Bad Request  — missing dest query parameter
//
// The decision is taken under the manager mutex against the most recently
// reconciled state cache. Worst-case latency between user-edit and the
// worker honouring it is one reconcile cycle (≈30 s).
func (m *MirrorManager) handleShouldMirror(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	expected := "Bearer " + m.workerToken
	if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	dest := r.URL.Query().Get("dest")
	if dest == "" {
		http.Error(w, "missing dest", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.imageState[dest]
	if !ok {
		// Not present in consolidated state → removed from spec.
		w.WriteHeader(http.StatusGone)
		return
	}
	if entry.State == stateMirrored {
		w.WriteHeader(http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// cleanupFinishedWorkers removes completed/failed worker pods.
// First it handles tracked pods in m.inProgress (resetting Failed images to
// Pending), then it sweeps for any orphaned finished pods that fell out of
// tracking (e.g. due to a manager restart).
// API calls (Get/Delete/List) are intentionally performed *outside* m.mu so
// network I/O does not block reconcile or status callbacks.
// Caller must NOT hold m.mu.
func (m *MirrorManager) cleanupFinishedWorkers(ctx context.Context) {
	// 1. Snapshot tracked pods under the lock.
	m.mu.Lock()
	snapshot := make(map[string]string, len(m.inProgress))
	for dest, podName := range m.inProgress {
		snapshot[dest] = podName
	}
	m.mu.Unlock()

	type finished struct {
		dest, podName string
		phase         corev1.PodPhase
	}
	done := make([]finished, 0, len(snapshot))
	deletedPods := map[string]struct{}{}

	// Fetch all worker pods in one go
	pods, err := m.Clientset.CoreV1().Pods(m.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=oc-mirror-worker,mirrortarget=%s", m.TargetName),
	})
	if err != nil {
		// If List fails, we log and return, as we can't reliably update states or delete pods
		oclog.Printf("Failed to list worker pods: %v\n", err)
		return
	}

	podsByName := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		podsByName[pod.Name] = pod
	}

	for dest, podName := range snapshot {
		pod, found := podsByName[podName]
		if !found {
			// Pod is gone (or unreachable) – drop from tracking.
			done = append(done, finished{dest: dest, podName: podName, phase: corev1.PodFailed})
			done[len(done)-1].phase = "" // signal "missing"
			continue
		}
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		done = append(done, finished{dest: dest, podName: podName, phase: pod.Status.Phase})
	}

	// 2. Mutate state under the lock.
	if len(done) > 0 {
		m.mu.Lock()
		for _, f := range done {
			// Only drop if the entry still maps to the same pod (avoid
			// racing with a freshly scheduled worker that reused the dest).
			if cur, ok := m.inProgress[f.dest]; ok && cur == f.podName {
				delete(m.inProgress, f.dest)
				if f.phase == corev1.PodFailed {
					oclog.Printf("Worker pod %s for %s failed without reporting; resetting to Pending\n", f.podName, f.dest)
					m.setImageStateLocked(f.dest, statePending, "")
				}
			}
		}
		ocmetrics.ManagerActiveWorkers.WithLabelValues(m.TargetName).Set(float64(len(m.inProgress)))
		m.mu.Unlock()
	}

	// 3. Delete finished pods (deduplicated, no lock held).
	for _, f := range done {
		if f.phase == "" {
			continue // already missing
		}
		if _, already := deletedPods[f.podName]; already {
			continue
		}
		_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, f.podName, metav1.DeleteOptions{})
		deletedPods[f.podName] = struct{}{}
	}

	// 4. Sweep for any orphaned finished worker pods not in m.inProgress.
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if _, already := deletedPods[pod.Name]; already {
			continue
		}
		oclog.Printf("Cleaning up orphaned worker pod %s (%s)\n", pod.Name, pod.Status.Phase)
		_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	}
}

// checkExistNoLock calls CheckExist for dest using the cached client. If the
// call fails with an HTTP 400 — bearer token scope too large for HAProxy/
// nginx proxies fronting some registries (e.g. Quay's nginx rejects tokens >
// ~8 KB once accumulated across enough repositories) — the cached client is
// discarded and the check is retried once against a fresh client with an
// empty token scope, since the accumulated scope, not the image itself, is
// almost always the cause.
//
// Unlike the rest of the manager's helpers, this does NOT touch m.mu at all:
// it is called concurrently by the background drift sweep (see
// startDriftSweepLocked) across many goroutines at once, purely to perform
// the network call. Callers apply the result under the lock separately.
func (m *MirrorManager) checkExistNoLock(ctx context.Context, dest string) (bool, error) {
	// Use cached client; ClientCache automatically refreshes every 5 minutes
	// to prevent auth token scope accumulation.
	checkClient, _ := m.clientCache.GetOrCreate(nil, m.authConfigPath)
	exists, checkErr := checkClient.CheckExist(ctx, dest)
	if checkErr == nil || !errors.Is(checkErr, errs.ErrHTTPStatus) || !strings.Contains(checkErr.Error(), "400") {
		return exists, checkErr
	}

	freshClient, _ := m.clientCache.RefreshClient(nil, m.authConfigPath)
	return freshClient.CheckExist(ctx, dest)
}

// driftSweepConcurrency bounds how many CheckExist calls the background
// drift sweep runs against the target registry in parallel. Chosen to make
// meaningful progress through MirrorTargets with tens of thousands of
// images without hammering the registry harder than the existing worker
// concurrency already does.
const driftSweepConcurrency = 20

// startDriftSweepLocked launches an asynchronous drift-check sweep over every
// currently Mirrored or PermanentlyFailed entry, unless one is already
// running. Caller must hold m.mu; the actual CheckExist calls happen in
// background goroutines that only briefly re-acquire m.mu per destination to
// read a snapshot and apply results — the sweep never blocks reconcile()'s
// own call stack.
//
// This replaces a previous design where the entire sweep ran inline inside a
// single reconcile() call: for a MirrorTarget with tens of thousands of
// images, that could take hours, during which Phase E (dispatching new
// worker batches) never ran even with idle worker capacity — mirroring
// progress visibly stalled for the whole sweep. Running it in the
// background instead means reconcile() keeps ticking normally (new pending
// images keep getting dispatched, status callbacks keep being handled)
// while the sweep makes progress concurrently, and results are applied
// incrementally as each check completes.
func (m *MirrorManager) startDriftSweepLocked(ctx context.Context, requireSignedByIS map[string]bool) {
	if m.driftSweepRunning {
		return
	}
	destinations := make([]string, 0, len(m.imageState))
	for dest, entry := range m.imageState {
		if entry == nil {
			continue
		}
		if entry.State == stateMirrored || (entry.State == stateFailed && entry.PermanentlyFailed) {
			destinations = append(destinations, dest)
		}
	}
	if len(destinations) == 0 {
		return
	}
	m.driftSweepRunning = true
	oclog.Printf("CheckExist: starting background drift sweep for %d images\n", len(destinations))
	go m.runDriftSweep(ctx, destinations, requireSignedByIS)
}

// runDriftSweep checks every destination in destinations against the target
// registry, bounded to driftSweepConcurrency in flight at once, and applies
// each result to the shared imagestate as it completes. Must be started via
// `go m.runDriftSweep(...)` — does not hold m.mu itself except briefly inside
// checkDriftOne.
func (m *MirrorManager) runDriftSweep(ctx context.Context, destinations []string, requireSignedByIS map[string]bool) {
	defer func() {
		m.mu.Lock()
		m.driftSweepRunning = false
		m.mu.Unlock()
		oclog.Println("CheckExist: background drift sweep complete")
	}()

	sem := make(chan struct{}, driftSweepConcurrency)
	var wg sync.WaitGroup
	for _, dest := range destinations {
		wg.Add(1)
		sem <- struct{}{}
		go func(dest string) {
			defer wg.Done()
			defer func() { <-sem }()
			m.checkDriftOne(ctx, dest, requireSignedByIS)
		}(dest)
	}
	wg.Wait()
}

// checkDriftOne verifies a single destination against the target registry
// and applies the result to the shared imagestate, mirroring the checks a
// synchronous sweep used to perform inline in reconcile()'s Phase D. Holds
// m.mu only for the brief snapshot-read and result-apply steps around the
// (lock-free) network call.
func (m *MirrorManager) checkDriftOne(ctx context.Context, dest string, requireSignedByIS map[string]bool) {
	m.mu.Lock()
	entry, ok := m.imageState[dest]
	if !ok || entry == nil || len(m.owners[dest]) == 0 {
		m.mu.Unlock()
		return
	}
	permanentlyFailedRecoveryCheck := entry.State == stateFailed && entry.PermanentlyFailed
	if entry.State != stateMirrored && !permanentlyFailedRecoveryCheck {
		// Spec narrowing, a fresh resolve, or a worker callback already
		// changed this entry since the sweep snapshot was taken — it's no
		// longer in a state this sweep is responsible for.
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	exists, checkErr := m.checkExistNoLock(ctx, dest)

	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok = m.imageState[dest]
	if !ok || entry == nil {
		return
	}

	if permanentlyFailedRecoveryCheck {
		// During CheckExist windows, verify the target registry. If not
		// found, reset for a fresh retry cycle (handles transient upstream
		// unavailability). PermanentlyFailed stays true so the
		// catalog-build gate remains open.
		if checkErr != nil {
			oclog.Printf("CheckExist error for permanently-failed image %s: %v – keeping Failed\n", dest, checkErr)
			return
		}
		if exists {
			oclog.Printf("Permanently-failed image %s found in target; marking Mirrored\n", dest)
			m.mirrored[dest] = true
			entry.State = stateMirrored
			entry.LastError = ""
			m.stateDirty = true
		} else {
			oclog.Printf("Permanently-failed image %s not in target; resetting for retry\n", dest)
			entry.State = statePending
			entry.RetryCount = 0 // fresh 10-attempt window; PermanentlyFailed stays true
			m.stateDirty = true
		}
		return
	}

	if checkErr != nil {
		oclog.Printf("CheckExist error for %s: %v – assuming present\n", dest, checkErr)
		m.mirrored[dest] = true
		return
	}
	if !exists {
		oclog.Printf("Image %s marked Mirrored but not found in registry; resetting to Pending\n", dest)
		entry.State = statePending
		entry.LastError = ""
		entry.RetryCount = 0
		m.stateDirty = true
		return
	}
	if m.additionalImageDriftedLocked(ctx, entry) {
		oclog.Printf("Additional image %s: upstream source %s has moved; resetting to Pending for re-mirror\n", dest, entry.Source)
		entry.State = statePending
		entry.LastError = ""
		entry.RetryCount = 0
		m.stateDirty = true
		return
	}
	m.mirrored[dest] = true
	if !entry.SignatureVerified && anyOwnerRequiresSignedImages(m.owners[dest], requireSignedByIS) {
		m.verifySignedImageLocked(ctx, dest, entry)
	}
}

// additionalImageDriftedLocked reports whether a tag-referenced additional
// image (imagestate.OriginAdditional) has changed upstream since it was
// mirrored, by comparing entry.Source's current manifest digest against
// entry.SourceDigest — the digest the worker resolved and reported at mirror
// time (see handleStatusUpdate).
//
// This only applies to additional images: release and operator destinations
// are content-addressed (mirror.ComponentDestination bakes the source digest
// into the destination path), so a changed upstream digest naturally produces
// a new destination and a fresh Pending entry — the existing orphan-cleanup
// path already handles the old one. Additional images mirror straight to a
// fixed, user-chosen destination (see Collector.CollectAdditional), so a
// mutable tag moving upstream is otherwise invisible: the destination never
// changes, and mergeIntoStateWithSig would keep preserving "Mirrored"
// forever. Digest-pinned sources ("@sha256:...") can't drift and are skipped.
//
// Always records the freshly resolved digest (even when unchanged) as the
// new baseline. A failed resolution is logged and treated as "not drifted"
// rather than forcing a spurious re-mirror; an entry that somehow has no
// baseline yet (e.g. state migrated from before this field existed) is
// likewise treated as "not drifted" on this check, since a comparison is
// meaningless without one — the freshly resolved digest recorded here
// becomes the baseline for the next window.
//
// Caller must hold m.mu; it is released for the duration of the network call.
func (m *MirrorManager) additionalImageDriftedLocked(ctx context.Context, entry *imagestate.ImageEntry) bool {
	if entry.Origin != imagestate.OriginAdditional || strings.Contains(entry.Source, "@sha256:") {
		return false
	}

	checkClient, _ := m.clientCache.GetOrCreate(nil, m.authConfigPath)
	m.mu.Unlock()
	digest, err := checkClient.GetDigest(ctx, entry.Source)
	m.mu.Lock()
	if err != nil {
		oclog.Printf("CheckExist: failed to resolve digest for additional image source %s: %v\n", entry.Source, err)
		return false
	}

	drifted := entry.SourceDigest != "" && entry.SourceDigest != digest
	if entry.SourceDigest != digest {
		entry.SourceDigest = digest
		m.stateDirty = true
	}
	return drifted
}

// anyOwnerRequiresSignedImages reports whether any ImageSet in owners has
// Mirror.RequireSignedImages set, per requireSignedByIS (built once per
// reconcile from the current ImageSet list).
func anyOwnerRequiresSignedImages(owners []string, requireSignedByIS map[string]bool) bool {
	for _, name := range owners {
		if requireSignedByIS[name] {
			return true
		}
	}
	return false
}

// destinationDigest extracts a "sha256:<hex>" digest from a mirrored
// destination reference, handling both conventions used across origins:
// digest-derived tags ("...repo:sha256-<hex>", mirror.ComponentDestination —
// release and operator images) and digest references
// ("...repo@sha256:<hex>", additional/helm images already digest-pinned at
// the source). Returns "" for tag-only destinations with no digest to check
// a cosign signature against — cosign signatures are digest-scoped, so
// RequireSignedImages cannot be enforced for those (logged, not failed).
func destinationDigest(dest string) string {
	if idx := strings.Index(dest, "@sha256:"); idx >= 0 {
		return dest[idx+1:]
	}
	const tagPrefix = ":sha256-"
	if idx := strings.LastIndex(dest, tagPrefix); idx >= 0 {
		return "sha256:" + dest[idx+len(tagPrefix):]
	}
	return ""
}

// verifySignedImageLocked checks whether dest carries a valid cosign
// signature, for entries whose owning ImageSet(s) have
// Mirror.RequireSignedImages set and that have not been verified yet. Called
// once per entry during the drift-check sweep (Phase D) — SignatureVerified
// then makes it a no-op on future sweeps.
//
// On success, sets entry.SignatureVerified. On failure, fails the entry via
// the normal retry lifecycle (State: Failed, RetryCount, PermanentlyFailed
// after 10 attempts) with a descriptive lastError, and clears
// m.mirrored[dest] so the next tick's "entry.State == stateMirrored" fast
// path does not fire and silently flip the entry back to Mirrored.
//
// Caller must hold m.mu; released for the duration of the network call.
func (m *MirrorManager) verifySignedImageLocked(ctx context.Context, dest string, entry *imagestate.ImageEntry) {
	digest := destinationDigest(dest)
	if digest == "" {
		oclog.Printf("Warning: %s has no digest to check a signature against; skipping RequireSignedImages check\n", dest)
		return
	}

	checkClient, _ := m.clientCache.GetOrCreate(nil, m.authConfigPath)
	m.mu.Unlock()
	sigErr := cosign.HasValidSignature(ctx, checkClient, dest, digest)
	m.mu.Lock()

	if sigErr == nil {
		entry.SignatureVerified = true
		m.stateDirty = true
		return
	}

	oclog.Printf("Signature check failed for %s: %v\n", dest, sigErr)
	entry.State = stateFailed
	entry.LastError = fmt.Sprintf("signature check failed: %v", sigErr)
	entry.SignatureVerified = false
	entry.RetryCount++
	ocmetrics.ManagerWorkerRetriesTotal.WithLabelValues(m.TargetName).Inc()
	if entry.RetryCount >= 10 && !entry.PermanentlyFailed {
		entry.PermanentlyFailed = true
	}
	m.mirrored[dest] = false
	m.stateDirty = true
	m.statusDirty = true
}

func (m *MirrorManager) reconcile(ctx context.Context) error { //nolint:gocyclo
	m.cleanupFinishedWorkers(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt); err != nil {
		return err
	}

	imageSets := &mirrorv1alpha1.ImageSetList{}
	if err := m.Client.List(ctx, imageSets, client.InNamespace(m.Namespace)); err != nil {
		return err
	}

	// Default concurrency=1 (one worker pod at a time) to avoid Quay blob
	// upload digest-mismatch errors. Quay's storage backend can corrupt
	// concurrent uploads of the same blob to different repositories. With
	// sequential processing, regclient's anonymous blob mount
	// (POST ?mount=<digest>) finds blobs pushed by earlier images in Quay's
	// global storage, skipping the upload entirely (zero-copy).
	concurrency := mt.Spec.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	batchSize := mt.Spec.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}

	// Phase A: Load partitioned state (per-ImageSet ConfigMaps + shared index)
	// once on first run or after restart. Worker callbacks update m.imageState
	// directly so we never reload from the ConfigMaps unless the cache is
	// empty (avoids overwriting RetryCount / PermanentlyFailed changes before
	// they are flushed).
	if len(m.imageState) == 0 {
		m.loadPartitionedState(ctx, mt, imageSets)
	}

	// Phase B: Per-IS resolution (may unlock mutex for network I/O).
	// The resolver does cheap network probes (manifest digest + Cincinnati
	// graph) and is gated via shouldResolve() so we don't hammer upstream
	// registries on every 30 s tick.
	justResolvedISes := map[string]bool{}
	// hadErrorISes marks ImageSets whose resolution this tick hit a transient
	// probe/collection error on at least one entry (see resolveImageSet's doc
	// comment). Phase G consults this to avoid advancing ObservedGeneration/
	// LastSuccessfulPollTime for those, so shouldResolve() retries again on
	// the very next tick instead of waiting up to a full pollInterval.
	hadErrorISes := map[string]bool{}
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		isView := filterByImageSet(m.imageState, m.owners, is.Name)
		if shouldResolve(&is, mt, isView) {
			isCopy := is.DeepCopy()
			isViewSnap := cloneImageState(isView)
			m.mu.Unlock()
			newPerISState, resolved, hadError, resolveErr := m.resolveImageSet(ctx, isCopy, mt, isViewSnap)
			m.mu.Lock()
			if resolveErr != nil {
				oclog.Printf("Warning: failed to resolve ImageSet %s: %v\n", is.Name, resolveErr)
			} else {
				// Merge any worker callbacks that fired during the unlock window.
				newPerISState = mergeWorkerUpdates(newPerISState, m.imageState)
				justResolvedISes[is.Name] = true
				hadErrorISes[is.Name] = hadError
				if resolved || len(isView) == 0 {
					// Returned orphans are re-derived by Phase D's zero-owner
					// sweep below (it also catches ones left over from a
					// crash between a previous tick's ownership update and
					// its flush); no need to track them here.
					_ = mergeResolvedIntoConsolidated(m.imageState, m.owners, newPerISState, is.Name)
					m.stateDirty = true
				}
			}
		}
	}

	// requireSignedByIS records, per currently-relevant ImageSet, whether
	// Mirror.RequireSignedImages is set — consulted below (and by the drift
	// sweep) so a shared destination is checked if ANY owning ImageSet
	// requires it.
	requireSignedByIS := make(map[string]bool, len(imageSets.Items))
	for _, is := range imageSets.Items {
		if containsString(mt.Spec.ImageSets, is.Name) {
			requireSignedByIS[is.Name] = is.Spec.Mirror.RequireSignedImages
		}
	}

	// Phase C: Drift check — launch a background sweep once per CheckExist
	// interval (see startDriftSweepLocked for why this runs asynchronously
	// rather than inline).
	checkExistInterval := 6 * time.Hour
	if mt.Spec.CheckExistInterval != nil && mt.Spec.CheckExistInterval.Duration >= time.Hour {
		checkExistInterval = mt.Spec.CheckExistInterval.Duration
	}
	if time.Since(m.lastDriftCheck) > checkExistInterval && !m.driftSweepRunning {
		m.lastDriftCheck = time.Now()
		// Refresh the cached client to avoid auth token scope accumulation.
		// Quay's nginx proxy returns 400 when the Bearer token exceeds ~8 KB.
		_, _ = m.clientCache.RefreshClient(nil, m.authConfigPath)
		m.startDriftSweepLocked(ctx, requireSignedByIS)
	}

	// Phase D: Process all entries — collect pending + sweep orphans. Drift
	// verification itself happens in the background (Phase C); entries are
	// trusted here and only reset to Pending once the sweep actually finds
	// them missing, at which point a later tick's pass through this loop
	// picks up the state change.
	pendingImages := make([]BatchItem, 0, len(m.imageState))
	newOrphans := make(imagestate.ImageState)

	for dest, entry := range m.imageState {
		// Orphaned entry (no ImageSet references it anymore — e.g. blocked via
		// spec.mirror.blockedImages, or dropped by spec narrowing): move it out
		// of the live working set into the pending-orphans snapshot for the
		// MirrorTarget controller's cleanup Job to pick up. Retrying or
		// drift-checking it here would keep re-mirroring an image nothing
		// references, exactly what blocking is meant to prevent.
		if len(m.owners[dest]) == 0 {
			newOrphans[dest] = entry
			delete(m.imageState, dest)
			delete(m.owners, dest)
			m.stateDirty = true
			continue
		}

		if entry.State == stateMirrored {
			m.mirrored[dest] = true
			continue
		}
		if m.mirrored[dest] {
			// Defensive sync: something (e.g. a worker callback landing
			// between two ticks) already confirmed this destination but the
			// entry's own State hadn't been updated yet. No network call —
			// just reconciling two in-memory views of the same fact.
			entry.State = stateMirrored
			m.stateDirty = true
			continue
		}

		if entry.State == stateFailed {
			if entry.RetryCount < 10 {
				// Transient failure: schedule immediate retry.
				entry.State = statePending
				m.stateDirty = true
			} else if !entry.PermanentlyFailed {
				// Permanently failed. Ensure the flag is persisted: it may
				// be missing from the ConfigMap if the manager restarted
				// before the dirty-flag flush ran (retryCount reached 10
				// but permanentlyFailed=true was not yet written). Recovery
				// checks against the registry for already-PermanentlyFailed
				// entries are handled by the background drift sweep.
				entry.PermanentlyFailed = true
				m.stateDirty = true
			}
			continue
		}

		if entry.State != statePending {
			continue
		}
		if m.inProgress[dest] != "" {
			continue
		}
		pendingImages = append(pendingImages, BatchItem{Source: entry.Source, Dest: dest})
	}

	if len(newOrphans) > 0 {
		if err := m.appendOrphans(ctx, mt, newOrphans); err != nil {
			oclog.Printf("Warning: failed to persist orphaned images: %v\n", err)
		}
	}

	// Phase E: Dispatch worker batches up to concurrency limit.
	activePods := map[string]struct{}{}
	for _, podName := range m.inProgress {
		activePods[podName] = struct{}{}
	}
	for i := 0; i < len(pendingImages) && len(activePods) < concurrency; i += batchSize {
		end := i + batchSize
		if end > len(pendingImages) {
			end = len(pendingImages)
		}
		batch := pendingImages[i:end]
		podName, startErr := m.startWorkerBatch(ctx, mt, batch)
		if startErr != nil {
			oclog.Printf("Failed to start worker batch: %v\n", startErr)
			ocmetrics.ManagerBatchesTotal.WithLabelValues(m.TargetName, "failed").Inc()
			continue
		}
		ocmetrics.ManagerBatchesTotal.WithLabelValues(m.TargetName, "success").Inc()
		for _, item := range batch {
			m.inProgress[item.Dest] = podName
		}
		activePods[podName] = struct{}{}
		ocmetrics.ManagerActiveWorkers.WithLabelValues(m.TargetName).Set(float64(len(activePods)))
		oclog.Printf("Started worker pod %s for batch of %d images\n", podName, len(batch))
	}

	// Phase F: Flush state to each owning ImageSet's own ConfigMap + the
	// shared-image index, replacing the single consolidated ConfigMap write.
	if m.stateDirty {
		if err := m.flushPartitionedState(ctx, mt); err != nil {
			oclog.Printf("Warning: failed to save partitioned state: %v\n", err)
			// stateDirty remains true; save will be retried on next tick.
		} else {
			m.stateDirty = false
		}
	}

	// Phase G: Update per-IS status from the live in-memory state (filtered
	// view). Guarded by statusDirty so we skip the Kubernetes API write when
	// nothing has changed since the last reconcile, reducing spurious
	// conflict retries.
	if m.statusDirty {
		for _, is := range imageSets.Items {
			if !containsString(mt.Spec.ImageSets, is.Name) {
				continue
			}
			isView := filterByImageSet(m.imageState, m.owners, is.Name)
			m.updateImageSetStatusLocked(ctx, &is, isView, justResolvedISes[is.Name] && !hadErrorISes[is.Name])
		}
		m.statusDirty = false
	}

	// Phase H: Generate and save global resources (IDMS, ITMS, CatalogSource) to ConfigMap.
	if err := m.saveGlobalResources(ctx, mt, imageSets); err != nil {
		oclog.Printf("Warning: failed to save global resources: %v\n", err)
	}

	return nil
}

// setImageStateLocked updates the in-memory state for a single destination
// and marks the consolidated state dirty so the next reconcile tick flushes
// the change to the ConfigMap. Caller must hold m.mu.
func (m *MirrorManager) setImageStateLocked(dest, st, lastError string) {
	entry, ok := m.imageState[dest]
	if !ok {
		return
	}
	// Idempotency: skip if already in the desired state with the same
	// error. Prevents duplicate HTTP retries from inflating RetryCount.
	if entry.State == st && entry.LastError == lastError {
		return
	}
	entry.State = st
	entry.LastError = lastError
	m.stateDirty = true
	m.statusDirty = true
	if st == stateFailed {
		entry.RetryCount++
		ocmetrics.ManagerWorkerRetriesTotal.WithLabelValues(m.TargetName).Inc()
		if entry.RetryCount >= 10 && !entry.PermanentlyFailed {
			entry.PermanentlyFailed = true
		}
	}
}

// flushPartitionedState writes m.imageState back out to each owning
// ImageSet's own ConfigMap plus the MirrorTarget's shared-image index,
// replacing the single consolidated ConfigMap write. All owners of a shared
// destination get the same values (State/RetryCount/etc. describe one
// underlying mirrored image, same as the pre-partitioning consolidated
// model) — multi-CM writes are not atomic (see
// docs/design/imagestate-per-imageset-partitioning.md §4), so a crash
// mid-flush can transiently leave one owner's copy stale; the next
// successful flush corrects it, and mergeLoadedEntry resolves any
// divergence observed on the next load.
// Caller must hold m.mu.
func (m *MirrorManager) flushPartitionedState(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	perImageSet := make(map[string]imagestate.ImageState, len(mt.Spec.ImageSets))
	for _, isName := range mt.Spec.ImageSets {
		perImageSet[isName] = make(imagestate.ImageState)
	}

	index := make(imagestate.SharedIndex)
	for dest, entry := range m.imageState {
		names := m.owners[dest]
		for _, isName := range names {
			state, ok := perImageSet[isName]
			if !ok {
				// isName owns this dest but is no longer in mt.Spec.ImageSets
				// (removal cleanup hasn't run yet) — still flush it so the
				// controller sees accurate state for the partition decision.
				state = make(imagestate.ImageState)
				perImageSet[isName] = state
			}
			state[dest] = entry
		}
		if len(names) > 1 {
			index[dest] = append([]string(nil), names...)
		}
	}

	var firstErr error
	for isName, state := range perImageSet {
		if err := imagestate.Save(ctx, m.Client, m.Namespace, isName, state, mt, m.Scheme); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("save state for imageset %s: %w", isName, err)
		}
	}
	if err := imagestate.SaveIndex(ctx, m.Client, m.Namespace, m.TargetName, index, mt, m.Scheme); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("save shared image index: %w", err)
	}
	return firstErr
}

// appendOrphans merges newOrphans into the MirrorTarget's pending-orphans
// ConfigMap, which the MirrorTarget controller consumes to create a cleanup
// Job for images dropped from every ImageSet that used to need them (spec
// narrowing or blocking) — the same way it does for a fully removed
// ImageSet, just sourced here instead of inferred by scanning a consolidated
// map (there no longer is one).
// Caller must hold m.mu.
func (m *MirrorManager) appendOrphans(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, newOrphans imagestate.ImageState) error {
	cmName := imagestate.OrphansConfigMapName(m.TargetName)
	existing, err := imagestate.LoadByConfigMapName(ctx, m.Client, m.Namespace, cmName)
	if err != nil {
		return fmt.Errorf("load pending orphans: %w", err)
	}
	for dest, entry := range newOrphans {
		existing[dest] = entry
	}
	return imagestate.SaveRaw(ctx, m.Client, m.Namespace, cmName, existing, mt, m.Scheme)
}

func (m *MirrorManager) saveGlobalResources(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSets *mirrorv1alpha1.ImageSetList) error {
	idms, err := resources.GenerateIDMS(m.TargetName, m.imageState)
	if err != nil {
		return fmt.Errorf("generate IDMS: %w", err)
	}

	itms, err := resources.GenerateITMS(m.TargetName, m.imageState)
	if err != nil {
		return fmt.Errorf("generate ITMS: %w", err)
	}

	data := map[string]string{
		"idms.yaml": string(idms),
		"itms.yaml": string(itms),
	}

	// Index the Operator spec for every catalog referenced by this
	// MirrorTarget's ImageSets, keyed by source catalog reference, so the
	// CatalogSource/ClusterCatalog target image can be computed with the
	// exact same TargetCatalog/TargetTag overrides the catalog-builder Job
	// used to push it (see internal/controller reconcileCatalogBuildJobs).
	opsBySource := make(map[string]mirrorv1alpha1.Operator)
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		for _, op := range is.Spec.Mirror.Operators {
			if op.Catalog == "" {
				continue
			}
			opsBySource[op.Catalog] = op
		}
	}

	// Generate CatalogSources for all unique catalogs in the state. Origin/
	// OriginRef are single flat values per entry (ownership lives in
	// m.owners, not on the entry) — for a destination shared by ImageSets
	// resolved from differently-labeled catalog entries, whichever ImageSet
	// first wrote the entry wins the displayed OriginRef; harmless here since
	// it only drives catalog/slug extraction, which is the same catalog
	// either way in the common case.
	catalogs := make(map[string]resources.CatalogInfo)
	for _, entry := range m.imageState {
		if entry.Origin != imagestate.OriginOperator || entry.OriginRef == "" {
			continue
		}
		// Extract catalog from OriginRef (hacky, but we don't store it explicitly
		// in entry). OriginRef format: "catalog [pkg1, pkg2]" or "catalog — bundle"
		parts := strings.Split(entry.OriginRef, " ")
		catSource := parts[0]
		if catSource == "" {
			continue
		}
		slug := resources.CatalogSlug(catSource)
		if _, ok := catalogs[slug]; ok {
			continue
		}
		// Fall back to a synthetic Operator{Catalog: catSource} when the
		// owning ImageSet's spec is no longer available (e.g. removed after
		// this state entry was recorded) — still correct for the common case
		// (no TargetCatalog/TargetTag).
		op, ok := opsBySource[catSource]
		if !ok {
			op = mirrorv1alpha1.Operator{Catalog: catSource}
		}
		catalogs[slug] = resources.CatalogInfo{
			SourceCatalog: catSource,
			TargetImage:   resources.CatalogTargetImage(mt.Spec.Registry, op),
			DisplayName:   slug,
		}
	}

	for slug, cat := range catalogs {
		cs, err := resources.GenerateCatalogSource(m.TargetName+"-"+slug, m.Namespace, cat, mt.Spec.AuthSecret)
		if err != nil {
			oclog.Printf("Warning: failed to generate CatalogSource for %s: %v\n", slug, err)
			continue
		}
		data[fmt.Sprintf("catalogsource-%s.yaml", slug)] = string(cs)

		cc, err := resources.GenerateClusterCatalog(m.TargetName+"-"+slug, cat)
		if err != nil {
			oclog.Printf("Warning: failed to generate ClusterCatalog for %s: %v\n", slug, err)
			continue
		}
		data[fmt.Sprintf("clustercatalog-%s.yaml", slug)] = string(cc)
	}

	// Generate index.json listing all resources.
	index := map[string][]string{
		"resources": {"idms.yaml", "itms.yaml"},
	}
	for key := range data {
		if strings.HasPrefix(key, "catalogsource-") || strings.HasPrefix(key, "clustercatalog-") {
			index["resources"] = append(index["resources"], key)
		}
	}
	sort.Strings(index["resources"])
	if indexData, err := json.MarshalIndent(index, "", "  "); err == nil {
		data["index.json"] = string(indexData)
	}

	cmName := fmt.Sprintf("oc-mirror-%s-resources", m.TargetName)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"oc-mirror.openshift.io/resource-config": m.TargetName,
			},
		},
		Data: data,
	}

	if err := controllerutil.SetControllerReference(mt, cm, m.Scheme); err != nil {
		oclog.Printf("Warning: failed to set owner on resources ConfigMap: %v\n", err)
	}

	existing := &corev1.ConfigMap{}
	err = m.Client.Get(ctx, client.ObjectKey{Name: cmName, Namespace: m.Namespace}, existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return m.Client.Create(ctx, cm)
	}

	existing.Data = cm.Data
	existing.Labels = cm.Labels
	return m.Client.Update(ctx, existing)
}

// updateImageSetStatusLocked updates the ImageSet status with aggregate counts
// filtered to entries that reference this ImageSet (isView = filterByImageSet
// result). The Ready condition is also refreshed.
//
// ObservedGeneration and LastSuccessfulPollTime are only advanced when
// justResolved is true, i.e. a resolve was attempted THIS tick and completed
// with no per-entry probe/collection errors (see the caller, manager.go
// Phase B/G, and resolveImageSet's doc comment). Status churn from worker
// callbacks does not reset the poll clock. Crucially, a resolve that hit a
// transient error and fell back to carrying over stale entries for one
// channel/catalog must NOT advance ObservedGeneration to the current
// Generation — doing so would make shouldResolve() believe this generation
// was fully handled, silently deferring a retry of the failed entry until
// the next full pollInterval (default 24h) instead of the next tick.
//
// Caller must hold m.mu.
func (m *MirrorManager) updateImageSetStatusLocked(ctx context.Context, is *mirrorv1alpha1.ImageSet, isView imagestate.ImageState, justResolved bool) {
	total, mirrored, pending, failed := imagestate.Counts(isView)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestIS := &mirrorv1alpha1.ImageSet{}
		if err := m.Client.Get(ctx, client.ObjectKeyFromObject(is), latestIS); err != nil {
			return err
		}

		latestIS.Status.TotalImages = total
		latestIS.Status.MirroredImages = mirrored
		latestIS.Status.PendingImages = pending
		latestIS.Status.FailedImages = failed
		if justResolved {
			latestIS.Status.ObservedGeneration = latestIS.Generation
			now := metav1.Now()
			latestIS.Status.LastSuccessfulPollTime = &now
		}

		details := make([]mirrorv1alpha1.FailedImageDetail, 0)
		for dest, entry := range isView {
			if entry == nil || !entry.PermanentlyFailed || entry.State == stateMirrored {
				continue
			}
			details = append(details, mirrorv1alpha1.FailedImageDetail{
				Source:      entry.Source,
				Destination: dest,
				Error:       entry.LastError,
				Origin:      entry.OriginRef,
			})
		}
		sort.Slice(details, func(i, j int) bool { return details[i].Destination < details[j].Destination })
		totalFailed := len(details)
		if totalFailed > maxFailedImageDetails {
			details = details[:maxFailedImageDetails]
		}
		latestIS.Status.FailedImageDetails = details

		readyStatus := metav1.ConditionTrue
		readyReason := "Collected"
		readyMsg := fmt.Sprintf("Collected %d images (%d mirrored, %d pending, %d failed)", total, mirrored, pending, failed)
		if totalFailed > maxFailedImageDetails {
			readyMsg += fmt.Sprintf(" — showing %d of %d permanently failed images", maxFailedImageDetails, totalFailed)
		}
		if total == 0 {
			readyStatus = metav1.ConditionFalse
			readyReason = "Empty"
			readyMsg = "no images resolved yet"
		}
		setReadyCondition(&latestIS.Status.Conditions, readyStatus, readyReason, readyMsg, latestIS.Generation)

		// Update the passed-in object as well so that the caller sees the changes
		is.Status = latestIS.Status

		return m.Client.Status().Update(ctx, latestIS)
	})
	if err != nil {
		oclog.Printf("Failed to update ImageSet %s status after retries: %v\n", is.Name, err)
	}
}

// setReadyCondition manages a "Ready" condition on the ImageSet. Local helper
// to avoid importing the controller package.
func setReadyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string, gen int64) {
	if conditions == nil {
		return
	}
	for i, c := range *conditions {
		if c.Type != conditionReady {
			continue
		}
		if c.Status != status || c.Reason != reason || c.Message != message || c.ObservedGeneration != gen {
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].ObservedGeneration = gen
			(*conditions)[i].LastTransitionTime = metav1.Now()
		}
		return
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	})
}

func (m *MirrorManager) startWorkerBatch(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, items []BatchItem) (string, error) {
	batchJSON, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("failed to encode batch: %w", err)
	}

	// Annotation stores just the destination refs for pod-recovery on manager restart.
	dests := make([]string, len(items))
	for i, item := range items {
		dests[i] = item.Dest
	}
	destsJSON, _ := json.Marshal(dests)

	managerHost := fmt.Sprintf("%s-manager.%s.svc.cluster.local", m.TargetName, m.Namespace)

	envVars := []corev1.EnvVar{
		{
			Name:  "MANAGER_URL",
			Value: fmt.Sprintf("http://%s:8080", managerHost),
		},
		{
			Name: "WORKER_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: m.workerTokenSecretName(),
					},
					Key: "token",
				},
			},
		},
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		{
			Name:  "MIRROR_BATCH",
			Value: string(batchJSON),
		},
	}

	var containerArgs []string
	if mt.Spec.Insecure {
		containerArgs = append(containerArgs, "--insecure")
	}

	var volumeMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	// Blob buffer volume for large image layers.  By default an emptyDir is
	// used.  When WorkerStorage is configured with a StorageClassName, a
	// generic ephemeral PVC is used instead.  Otherwise, emptyDir with the
	// configured size is used.
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "blob-buffer",
		MountPath: "/tmp/blob-buffer",
	})

	blobBufferSize := resource.MustParse("10Gi")
	if ws := mt.Spec.WorkerStorage; ws != nil && !ws.Size.IsZero() {
		blobBufferSize = ws.Size
	}

	if ws := mt.Spec.WorkerStorage; ws != nil && ws.StorageClassName != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "blob-buffer",
			VolumeSource: corev1.VolumeSource{
				Ephemeral: &corev1.EphemeralVolumeSource{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							StorageClassName: ws.StorageClassName,
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: blobBufferSize,
								},
							},
						},
					},
				},
			},
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: "blob-buffer",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: &blobBufferSize,
				},
			},
		})
	}

	if mt.Spec.AuthSecret != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "DOCKER_CONFIG",
			Value: "/run/secrets/dockerconfig",
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "dockerconfig",
			MountPath: "/run/secrets/dockerconfig",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "dockerconfig",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mt.Spec.AuthSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
	}

	// Inject proxy env vars when a proxy is configured.
	envVars = append(envVars, workerProxyEnvVars(mt.Spec.Proxy)...)

	// Inject CA bundle when configured.
	if mt.Spec.CABundle != nil {
		caKey := mt.Spec.CABundle.Key
		if caKey == "" {
			caKey = "ca-bundle.crt"
		}
		envVars = append(envVars, corev1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: "/run/secrets/ca/" + caKey,
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "ca-bundle",
			MountPath: "/run/secrets/ca",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "ca-bundle",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: mt.Spec.CABundle.ConfigMapName,
					},
					Items: []corev1.KeyToPath{
						{Key: caKey, Path: caKey},
					},
				},
			},
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-worker-", mt.Name),
			Namespace:    m.Namespace,
			Labels: map[string]string{
				"app":          "oc-mirror-worker",
				"mirrortarget": m.TargetName,
			},
			Annotations: map[string]string{
				"mirror.openshift.io/destinations": string(destsJSON),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: m.TargetName + "-worker",
			ImagePullSecrets:   []corev1.LocalObjectReference{{Name: mt.Spec.AuthSecret}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: pointerTo(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "worker",
					Image: m.Image,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: pointerTo(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Args:         containerArgs,
					Env:          envVars,
					VolumeMounts: volumeMounts,
					Resources:    mt.Spec.Worker.Resources,
				},
			},
			Volumes:      volumes,
			NodeSelector: mt.Spec.Worker.NodeSelector,
			Tolerations:  mt.Spec.Worker.Tolerations,
		},
	}

	if err := controllerutil.SetControllerReference(mt, pod, m.Scheme); err != nil {
		return "", fmt.Errorf("failed to set owner reference: %w", err)
	}

	created, err := m.Clientset.CoreV1().Pods(m.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

func pointerTo[T any](v T) *T {
	return &v
}

// clusterNoProxy contains address patterns that always bypass the proxy so that
// pod-to-service traffic via cluster-internal FQDNs is never routed through an
// external proxy.  Kept in sync with the controller's clusterNoProxy.
var clusterNoProxy = []string{
	"localhost",
	"127.0.0.1",
	".svc",
	".svc.cluster.local",
}

// workerProxyEnvVars returns HTTP/HTTPS/NO_PROXY environment variables (both
// upper and lower case) for the given proxy configuration so that tools that
// only check one variant still see the proxy.  Returns nil when cfg is nil.
// When a proxy is configured, clusterNoProxy entries are automatically prepended
// to NO_PROXY, and KUBERNETES_SERVICE_HOST is overridden to the FQDN so that
// client-go's in-cluster config bypasses the proxy.
func workerProxyEnvVars(cfg *mirrorv1alpha1.ProxyConfig) []corev1.EnvVar {
	if cfg == nil {
		return nil
	}
	var env []corev1.EnvVar
	if v := cfg.HTTPProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: v},
			corev1.EnvVar{Name: "http_proxy", Value: v},
		)
	}
	if v := cfg.HTTPSProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: v},
			corev1.EnvVar{Name: "https_proxy", Value: v},
		)
	}
	if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
		noProxy := workerBuildEffectiveNoProxy(cfg.NoProxy)
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: noProxy},
			corev1.EnvVar{Name: "no_proxy", Value: noProxy},
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: "kubernetes.default.svc.cluster.local"},
		)
	} else if v := cfg.NoProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: v},
			corev1.EnvVar{Name: "no_proxy", Value: v},
		)
	}
	return env
}

// workerBuildEffectiveNoProxy prepends clusterNoProxy to userNoProxy.
func workerBuildEffectiveNoProxy(userNoProxy string) string {
	base := strings.Join(clusterNoProxy, ",")
	if userNoProxy == "" {
		return base
	}
	return base + "," + userNoProxy
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
