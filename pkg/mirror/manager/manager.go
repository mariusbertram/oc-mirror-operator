package manager

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
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

	workerToken string

	// State in memory — protected by mu
	mu             sync.RWMutex
	inProgress     map[string]string     // dest → podName
	mirrored       map[string]bool       // dest → true once successfully mirrored
	imageState     imagestate.ImageState // consolidated per-MirrorTarget state
	lastDriftCheck time.Time             // last time we verified all mirrored images
	stateDirty     bool                  // true when imageState has unsaved changes
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

	image := os.Getenv("OPERATOR_IMAGE")
	if image == "" {
		return nil, fmt.Errorf("OPERATOR_IMAGE environment variable is required but not set")
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
		// workerToken is populated lazily by ensureWorkerTokenSecret() in Run().
		inProgress: make(map[string]string),
		mirrored:   make(map[string]bool),
		imageState: make(imagestate.ImageState),
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
	if !errors.IsNotFound(getErr) {
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
	fmt.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

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
		fmt.Printf("Warning: could not sync in-progress state from pods: %v\n", err)
	}

	// Start Status API Server (internal, port 8080)
	go m.runStatusAPI(ctx)

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
			fmt.Printf("Cleaning up finished worker pod %s (%s)\n", pod.Name, pod.Status.Phase)
			_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			continue
		}
		// New multi-dest annotation (batch mode)
		if destsJSON, ok := pod.Annotations["mirror.openshift.io/destinations"]; ok && destsJSON != "" {
			var dests []string
			if json.Unmarshal([]byte(destsJSON), &dests) == nil {
				for _, dest := range dests {
					m.inProgress[dest] = pod.Name
					fmt.Printf("Recovered in-progress worker %s for %s\n", pod.Name, dest)
				}
				continue
			}
		}
		// Backward compat: legacy single-dest annotation
		if dest, ok := pod.Annotations["mirror.openshift.io/destination"]; ok && dest != "" {
			m.inProgress[dest] = pod.Name
			fmt.Printf("Recovered in-progress worker %s for %s\n", pod.Name, dest)
		}
	}
	return nil
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
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Status API server failed: %v\n", err)
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

	fmt.Printf("Received status update from %s for %s\n", req.PodName, req.Destination)

	if req.Error != "" {
		m.setImageStateLocked(req.Destination, stateFailed, req.Error)
	} else {
		m.mirrored[req.Destination] = true
		m.setImageStateLocked(req.Destination, stateMirrored, "")
	}

	// Remove from in-progress tracking. The pod itself is cleaned up by
	// cleanupFinishedWorkers() once it reaches Succeeded/Failed, so that
	// other batch items in the same pod can continue reporting.
	delete(m.inProgress, req.Destination)

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

	for dest, podName := range snapshot {
		pod, err := m.Clientset.CoreV1().Pods(m.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
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
					fmt.Printf("Worker pod %s for %s failed without reporting; resetting to Pending\n", f.podName, f.dest)
					m.setImageStateLocked(f.dest, statePending, "")
				}
			}
		}
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
	pods, err := m.Clientset.CoreV1().Pods(m.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=oc-mirror-worker,mirrortarget=%s", m.TargetName),
	})
	if err != nil {
		return
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if _, already := deletedPods[pod.Name]; already {
			continue
		}
		fmt.Printf("Cleaning up orphaned worker pod %s (%s)\n", pod.Name, pod.Status.Phase)
		_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	}
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

	// Phase A: Load consolidated state once on first run or after restart.
	// Worker callbacks update m.imageState directly so we never reload from
	// the ConfigMap unless the cache is empty (avoids overwriting RetryCount /
	// PermanentlyFailed changes before they are flushed).
	if len(m.imageState) == 0 {
		if err := m.loadConsolidatedState(ctx, mt, imageSets); err != nil {
			fmt.Printf("Warning: failed to load consolidated state: %v\n", err)
		}
	}

	// Phase B: Per-IS resolution (may unlock mutex for network I/O).
	// The resolver does cheap network probes (manifest digest + Cincinnati
	// graph) and is gated via shouldResolve() so we don't hammer upstream
	// registries on every 30 s tick.
	justResolvedISes := map[string]bool{}
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		isView := filterByImageSet(m.imageState, is.Name)
		if shouldResolve(&is, mt, isView) {
			isCopy := is.DeepCopy()
			isViewSnap := cloneImageState(isView)
			m.mu.Unlock()
			newPerISState, resolved, resolveErr := m.resolveImageSet(ctx, isCopy, mt, isViewSnap)
			m.mu.Lock()
			if resolveErr != nil {
				fmt.Printf("Warning: failed to resolve ImageSet %s: %v\n", is.Name, resolveErr)
			} else {
				// Merge any worker callbacks that fired during the unlock window.
				newPerISState = mergeWorkerUpdates(newPerISState, m.imageState)
				justResolvedISes[is.Name] = true
				if resolved || len(isView) == 0 {
					mergeResolvedIntoConsolidated(m.imageState, newPerISState, is.Name)
					m.stateDirty = true
				}
			}
		}
	}

	// Phase C: Drift check setup — reset once per CheckExist interval.
	checkExistInterval := 6 * time.Hour
	if mt.Spec.CheckExistInterval != nil && mt.Spec.CheckExistInterval.Duration >= time.Hour {
		checkExistInterval = mt.Spec.CheckExistInterval.Duration
	}
	driftCheckActive := time.Since(m.lastDriftCheck) > checkExistInterval
	if driftCheckActive {
		m.mirrored = make(map[string]bool)
		m.lastDriftCheck = time.Now()
		// Create a fresh regclient to avoid auth token scope accumulation.
		// Quay's nginx proxy returns 400 when the Bearer token exceeds ~8 KB.
		m.mirrorClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
		fmt.Println("CheckExist: verifying images in target registry")
	}

	// Phase D: Process all consolidated entries — drift check + collect pending.
	pendingImages := make([]BatchItem, 0, len(m.imageState))
	checkClient := mirrorclient.NewMirrorClient(nil, m.authConfigPath)
	checkCount := 0

	for dest, entry := range m.imageState {
		// For images marked Mirrored in the ConfigMap but not yet verified
		// in memory: check the registry during CheckExist windows to confirm
		// they still exist (drift detection).
		if entry.State == stateMirrored && !m.mirrored[dest] {
			if !driftCheckActive {
				// Outside check window: trust the ConfigMap state.
				m.mirrored[dest] = true
				continue
			}
			// Refresh the client every 20 checks to prevent auth token
			// scope accumulation (Quay's nginx rejects tokens > ~8 KB).
			checkCount++
			if checkCount%20 == 0 {
				checkClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
			}
			// Release the manager lock while making the HTTP call so
			// status callbacks from worker pods are not blocked on
			// remote registry latency.
			m.mu.Unlock()
			exists, checkErr := checkClient.CheckExist(ctx, dest)
			m.mu.Lock()
			if checkErr != nil {
				fmt.Printf("CheckExist error for %s: %v – assuming present\n", dest, checkErr)
				m.mirrored[dest] = true
				continue
			}
			if exists {
				m.mirrored[dest] = true
				continue
			}
			fmt.Printf("Image %s marked Mirrored but not found in registry; resetting to Pending\n", dest)
			entry.State = statePending
			entry.LastError = ""
			entry.RetryCount = 0
			m.stateDirty = true
		}

		if m.mirrored[dest] {
			if entry.State != stateMirrored {
				entry.State = stateMirrored
				m.stateDirty = true
			}
			continue
		}

		if entry.State == stateFailed {
			if entry.RetryCount < 10 {
				// Transient failure: schedule immediate retry.
				entry.State = statePending
				m.stateDirty = true
			} else {
				// Permanently failed. Ensure the flag is persisted: it may
				// be missing from the ConfigMap if the manager restarted
				// before the dirty-flag flush ran (retryCount reached 10
				// but permanentlyFailed=true was not yet written).
				if !entry.PermanentlyFailed {
					entry.PermanentlyFailed = true
					m.stateDirty = true
				}
				if driftCheckActive {
					// During CheckExist windows, verify the target registry.
					// If not found, reset for a fresh retry cycle (handles
					// transient upstream unavailability). PermanentlyFailed
					// stays true so the catalog-build gate remains open.
					checkCount++
					if checkCount%20 == 0 {
						checkClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
					}
					m.mu.Unlock()
					exists, checkErr := checkClient.CheckExist(ctx, dest)
					m.mu.Lock()
					if checkErr != nil {
						fmt.Printf("CheckExist error for permanently-failed image %s: %v – keeping Failed\n", dest, checkErr)
					} else if exists {
						fmt.Printf("Permanently-failed image %s found in target; marking Mirrored\n", dest)
						m.mirrored[dest] = true
						entry.State = stateMirrored
						entry.LastError = ""
						m.stateDirty = true
					} else {
						fmt.Printf("Permanently-failed image %s not in target; resetting for retry\n", dest)
						entry.State = statePending
						entry.RetryCount = 0 // fresh 10-attempt window; PermanentlyFailed stays true
						m.stateDirty = true
					}
				}
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
			fmt.Printf("Failed to start worker batch: %v\n", startErr)
			continue
		}
		for _, item := range batch {
			m.inProgress[item.Dest] = podName
		}
		activePods[podName] = struct{}{}
		fmt.Printf("Started worker pod %s for batch of %d images\n", podName, len(batch))
	}

	// Phase F: Flush consolidated state to ConfigMap.
	if m.stateDirty {
		if err := imagestate.SaveForTarget(ctx, m.Client, m.Namespace, m.TargetName, m.imageState); err != nil {
			fmt.Printf("Warning: failed to save consolidated state: %v\n", err)
			// stateDirty remains true; save will be retried on next tick.
		} else {
			m.stateDirty = false
		}
	}

	// Phase G: Update per-IS status from consolidated state (filtered view).
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		isView := filterByImageSet(m.imageState, is.Name)
		m.updateImageSetStatusLocked(ctx, &is, isView, justResolvedISes[is.Name])
	}

	// Phase H: Generate and save global resources (IDMS, ITMS, CatalogSource) to ConfigMap.
	if err := m.saveGlobalResources(ctx, mt); err != nil {
		fmt.Printf("Warning: failed to save global resources: %v\n", err)
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
	if st == stateFailed {
		entry.RetryCount++
		if entry.RetryCount >= 10 && !entry.PermanentlyFailed {
			entry.PermanentlyFailed = true
		}
	}
}

func (m *MirrorManager) saveGlobalResources(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
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

	// Generate CatalogSources for all unique catalogs in the state.
	catalogs := make(map[string]resources.CatalogInfo)
	for _, entry := range m.imageState {
		// Check both legacy flat fields and consolidated refs.
		refs := entry.Refs
		if len(refs) == 0 {
			// Backward compat: use flat fields as a single virtual ref.
			refs = []imagestate.ImageRef{{
				Origin:    entry.Origin,
				OriginRef: entry.OriginRef,
			}}
		}

		for _, ref := range refs {
			if ref.Origin == imagestate.OriginOperator {
				// Extract catalog from OriginRef (hacky, but we don't store it explicitly in entry)
				// OriginRef format: "catalog [pkg1, pkg2]" or "catalog — bundle"
				if ref.OriginRef == "" {
					continue
				}
				parts := strings.Split(ref.OriginRef, " ")
				catSource := parts[0]
				if catSource == "" {
					continue
				}
				slug := resources.CatalogSlug(catSource)
				if _, ok := catalogs[slug]; !ok {
					catalogs[slug] = resources.CatalogInfo{
						SourceCatalog: catSource,
						TargetImage:   resources.CatalogTargetImage(mt.Spec.Registry, catSource),
						DisplayName:   slug,
					}
				}
			}
		}
	}

	for slug, cat := range catalogs {
		cs, err := resources.GenerateCatalogSource(m.TargetName+"-"+slug, m.Namespace, cat, mt.Spec.AuthSecret)
		if err != nil {
			fmt.Printf("Warning: failed to generate CatalogSource for %s: %v\n", slug, err)
			continue
		}
		data[fmt.Sprintf("catalogsource-%s.yaml", slug)] = string(cs)

		cc, err := resources.GenerateClusterCatalog(m.TargetName+"-"+slug, cat)
		if err != nil {
			fmt.Printf("Warning: failed to generate ClusterCatalog for %s: %v\n", slug, err)
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
// result). ObservedGeneration and the Ready condition are also refreshed.
//
// LastSuccessfulPollTime is updated only when justResolved is true, so the
// pollInterval gate in shouldResolve() works correctly. Status churn from
// worker callbacks does not reset the poll clock.
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
		latestIS.Status.ObservedGeneration = latestIS.Generation
		if justResolved {
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
		fmt.Printf("Failed to update ImageSet %s status after retries: %v\n", is.Name, err)
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

	containerArgs := []string{"worker"}
	if mt.Spec.Insecure {
		containerArgs = append(containerArgs, "--insecure")
	}

	var volumeMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	// Blob buffer volume for large image layers.  By default an emptyDir is
	// used.  When WorkerStorage is configured, a generic ephemeral PVC is
	// used instead — Kubernetes binds the PVC to the pod lifecycle so no
	// explicit cleanup is needed.
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "blob-buffer",
		MountPath: "/tmp/blob-buffer",
	})
	if ws := mt.Spec.WorkerStorage; ws != nil {
		size := ws.Size
		if size.IsZero() {
			size = resource.MustParse("10Gi")
		}
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
									corev1.ResourceStorage: size,
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
					SizeLimit: resourcePtr("10Gi"),
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

// resourcePtr parses a resource quantity string and returns a pointer to it.
// Used for EmptyDir size limits.
func resourcePtr(q string) *resource.Quantity {
	v := resource.MustParse(q)
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
