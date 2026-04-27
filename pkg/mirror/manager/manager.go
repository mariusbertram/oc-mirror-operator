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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// Image entry state constants used throughout the manager.
const (
	stateMirrored = "Mirrored"
	statePending  = "Pending"
	stateFailed   = "Failed"
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
	imageState     imagestate.ImageState // consolidated state across all ImageSets
	lastDriftCheck time.Time             // last time we verified all mirrored images
	stateDirty     bool                  // true if state needs a ConfigMap save
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
// yet exist.
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

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt); err != nil {
		return fmt.Errorf("load MirrorTarget for bootstrap: %w", err)
	}
	if err := m.ensureWorkerTokenSecret(ctx, mt); err != nil {
		return fmt.Errorf("worker token bootstrap: %w", err)
	}

	// Phase 3: Migrate old per-ImageSet ConfigMaps to the consolidated state.
	if err := m.migrateOldConfigMaps(ctx, mt); err != nil {
		fmt.Printf("Warning: state migration failed: %v\n", err)
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

// migrateOldConfigMaps converts per-IS state to consolidated target state.
func (m *MirrorManager) migrateOldConfigMaps(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	consolidated, err := imagestate.LoadForTarget(ctx, m.Client, m.Namespace, m.TargetName)
	if err != nil {
		return fmt.Errorf("load consolidated state: %w", err)
	}

	migratedAny := false
	for _, isName := range mt.Spec.ImageSets {
		cmName := imagestate.ConfigMapName(isName)
		cm := &corev1.ConfigMap{}
		err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: cmName}, cm)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			fmt.Printf("Warning: could not get ConfigMap %s: %v\n", cmName, err)
			continue
		}

		if cm.Annotations != nil && cm.Annotations["mirror.openshift.io/migrated"] == "true" {
			continue
		}

		fmt.Printf("Migrating old image state from %s to consolidated target state\n", cmName)
		oldState, _, err := imagestate.LoadWithExistence(ctx, m.Client, m.Namespace, isName)
		if err != nil {
			fmt.Printf("Warning: could not load state from %s: %v\n", cmName, err)
			continue
		}

		for dest, oldEntry := range oldState {
			entry, ok := consolidated[dest]
			if !ok {
				entry = &imagestate.ImageEntry{
					Source:            oldEntry.Source,
					State:             oldEntry.State,
					LastError:         oldEntry.LastError,
					RetryCount:        oldEntry.RetryCount,
					PermanentlyFailed: oldEntry.PermanentlyFailed,
				}
				consolidated[dest] = entry
			}
			entry.AddRef(imagestate.ImageRef{
				ImageSet:  isName,
				Origin:    oldEntry.Origin,
				EntrySig:  oldEntry.EntrySig,
				OriginRef: oldEntry.OriginRef,
			})
		}

		if cm.Annotations == nil {
			cm.Annotations = make(map[string]string)
		}
		cm.Annotations["mirror.openshift.io/migrated"] = "true"
		if err := m.Client.Update(ctx, cm); err != nil {
			fmt.Printf("Warning: could not mark %s as migrated: %v\n", cmName, err)
		}
		migratedAny = true
	}

	if migratedAny {
		if err := imagestate.SaveForTarget(ctx, m.Client, m.Namespace, mt, consolidated); err != nil {
			return fmt.Errorf("save migrated consolidated state: %w", err)
		}
		m.mu.Lock()
		m.imageState = consolidated
		m.mu.Unlock()
		fmt.Printf("State migration completed successfully for MirrorTarget %s\n", mt.Name)
	}

	return nil
}

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

	delete(m.inProgress, req.Destination)
	w.WriteHeader(http.StatusOK)
}

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
		w.WriteHeader(http.StatusGone)
		return
	}
	switch entry.State {
	case stateMirrored:
		w.WriteHeader(http.StatusGone)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (m *MirrorManager) cleanupFinishedWorkers(ctx context.Context) {
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
			done = append(done, finished{dest: dest, podName: podName})
			continue
		}
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		done = append(done, finished{dest: dest, podName: podName, phase: pod.Status.Phase})
	}

	if len(done) > 0 {
		m.mu.Lock()
		for _, f := range done {
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

	for _, f := range done {
		if _, already := deletedPods[f.podName]; already {
			continue
		}
		_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, f.podName, metav1.DeleteOptions{})
		deletedPods[f.podName] = struct{}{}
	}

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

func (m *MirrorManager) reconcile(ctx context.Context) error {
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

	// 1. Load state if needed.
	if len(m.imageState) == 0 {
		loaded, loadErr := imagestate.LoadForTarget(ctx, m.Client, m.Namespace, m.TargetName)
		if loadErr != nil {
			fmt.Printf("Warning: failed to load consolidated image state: %v\n", loadErr)
		} else {
			m.imageState = loaded
		}
	}

	// 2. Reconcile ImageSets and identify active ones.
	activeImageSets := m.reconcileImageSets(ctx, mt, imageSets)

	// 3. Handle drift detection and identify pending images.
	pendingImages := m.reconcileDriftAndPending(ctx, mt)

	// 4. Dispatch batches.
	m.dispatchWorkers(ctx, mt, pendingImages)

	// 5. Finalize state and status.
	if m.stateDirty {
		if err := imagestate.SaveForTarget(ctx, m.Client, m.Namespace, mt, m.imageState); err != nil {
			fmt.Printf("Warning: failed to save consolidated image state: %v\n", err)
		} else {
			m.stateDirty = false
		}
	}

	m.updateStatusLocked(ctx, mt, activeImageSets)

	// Phase 7a: Render and persist cluster resources to ConfigMaps.
	if err := m.renderResources(ctx, mt, activeImageSets); err != nil {
		fmt.Printf("Warning: failed to render resources: %v\n", err)
	}

	return nil
}

func (m *MirrorManager) reconcileImageSets(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSets *mirrorv1alpha1.ImageSetList) []*mirrorv1alpha1.ImageSet {
	activeImageSets := make([]*mirrorv1alpha1.ImageSet, 0, len(imageSets.Items))
	for i := range imageSets.Items {
		is := &imageSets.Items[i]
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		activeImageSets = append(activeImageSets, is)

		if shouldResolve(is, mt, m.imageState) {
			isCopy := is.DeepCopy()
			stateSnap := cloneImageState(m.imageState)
			m.mu.Unlock()
			newState, resolved, resolveErr := m.resolveImageSet(ctx, isCopy, mt, stateSnap)
			m.mu.Lock()
			if resolveErr != nil {
				fmt.Printf("Warning: failed to resolve ImageSet %s: %v\n", is.Name, resolveErr)
			} else if resolved {
				m.imageState = mergeWorkerUpdates(newState, m.imageState)
				m.stateDirty = true
				fmt.Printf("Resolved ImageSet %s\n", is.Name)
			}
		}
	}
	return activeImageSets
}

func (m *MirrorManager) reconcileDriftAndPending(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) []BatchItem {
	checkExistInterval := 6 * time.Hour
	if mt.Spec.CheckExistInterval != nil && mt.Spec.CheckExistInterval.Duration >= time.Hour {
		checkExistInterval = mt.Spec.CheckExistInterval.Duration
	}
	driftCheckActive := time.Since(m.lastDriftCheck) > checkExistInterval
	if driftCheckActive {
		m.mirrored = make(map[string]bool)
		m.lastDriftCheck = time.Now()
		m.mirrorClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
		fmt.Println("CheckExist: verifying images in target registry")
	}

	pendingImages := make([]BatchItem, 0, len(m.imageState))
	checkClient := mirrorclient.NewMirrorClient(nil, m.authConfigPath)
	checkCount := 0

	for dest, entry := range m.imageState {
		if m.checkDriftLocked(ctx, dest, entry, driftCheckActive, &checkCount, &checkClient) {
			continue
		}

		if entry.State == stateFailed {
			m.handleFailedEntryLocked(ctx, dest, entry, driftCheckActive, &checkCount, &checkClient)
			continue
		}

		if entry.State == statePending && m.inProgress[dest] == "" {
			pendingImages = append(pendingImages, BatchItem{Source: entry.Source, Dest: dest})
		}
	}
	return pendingImages
}

func (m *MirrorManager) checkDriftLocked(ctx context.Context, dest string, entry *imagestate.ImageEntry, driftActive bool, checkCount *int, checkClient **mirrorclient.MirrorClient) bool {
	if entry.State == stateMirrored && !m.mirrored[dest] {
		if !driftActive {
			m.mirrored[dest] = true
			return true
		}
		*checkCount++
		if *checkCount%20 == 0 {
			*checkClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
		}
		m.mu.Unlock()
		exists, err := (*checkClient).CheckExist(ctx, dest)
		m.mu.Lock()
		if err != nil {
			m.mirrored[dest] = true
			return true
		}
		if exists {
			m.mirrored[dest] = true
			return true
		}
		entry.State, entry.LastError, entry.RetryCount = statePending, "", 0
		m.stateDirty = true
	}
	if m.mirrored[dest] {
		if entry.State != stateMirrored {
			entry.State = stateMirrored
			m.stateDirty = true
		}
		return true
	}
	return false
}

func (m *MirrorManager) handleFailedEntryLocked(ctx context.Context, dest string, entry *imagestate.ImageEntry, driftActive bool, checkCount *int, checkClient **mirrorclient.MirrorClient) {
	if entry.RetryCount < 10 {
		entry.State = statePending
		m.stateDirty = true
		return
	}
	if !entry.PermanentlyFailed {
		entry.PermanentlyFailed = true
		m.stateDirty = true
	}
	if driftActive {
		*checkCount++
		if *checkCount%20 == 0 {
			*checkClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
		}
		m.mu.Unlock()
		exists, _ := (*checkClient).CheckExist(ctx, dest)
		m.mu.Lock()
		if exists {
			m.mirrored[dest] = true
			entry.State, entry.LastError = stateMirrored, ""
			m.stateDirty = true
		} else {
			entry.State, entry.RetryCount = statePending, 0
			m.stateDirty = true
		}
	}
}

func (m *MirrorManager) dispatchWorkers(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, pendingImages []BatchItem) {
	concurrency, batchSize := mt.Spec.Concurrency, mt.Spec.BatchSize
	if concurrency <= 0 {
		concurrency = 1
	}
	if batchSize <= 0 {
		batchSize = 50
	}

	activePods := make(map[string]struct{})
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
}

func (m *MirrorManager) setImageStateLocked(dest, st, lastError string) {
	entry, ok := m.imageState[dest]
	if !ok {
		return
	}
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

func (m *MirrorManager) updateStatusLocked(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, activeIS []*mirrorv1alpha1.ImageSet) {
	total, mirrored, pending, failed := imagestate.Counts(m.imageState)
	mt.Status.TotalImages = total
	mt.Status.MirroredImages = mirrored
	mt.Status.PendingImages = pending
	mt.Status.FailedImages = failed

	if err := m.Client.Status().Update(ctx, mt); err != nil {
		fmt.Printf("Failed to update MirrorTarget status: %v\n", err)
	}

	for _, is := range activeIS {
		isTotal, isMirrored, isPending, isFailed := imagestate.CountsForImageSet(m.imageState, is.Name)
		is.Status.TotalImages = isTotal
		is.Status.MirroredImages = isMirrored
		is.Status.PendingImages = isPending
		is.Status.FailedImages = isFailed
		is.Status.ObservedGeneration = is.Generation

		details := make([]mirrorv1alpha1.FailedImageDetail, 0)
		for dest, entry := range m.imageState {
			if entry == nil || !entry.PermanentlyFailed || entry.State == stateMirrored {
				continue
			}
			if !entry.HasImageSet(is.Name) {
				continue
			}

			var originRef string
			for _, ref := range entry.Refs {
				if ref.ImageSet == is.Name {
					originRef = ref.OriginRef
					break
				}
			}

			details = append(details, mirrorv1alpha1.FailedImageDetail{
				Source:      entry.Source,
				Destination: dest,
				Error:       entry.LastError,
				Origin:      originRef,
			})
		}
		sort.Slice(details, func(i, j int) bool { return details[i].Destination < details[j].Destination })
		if len(details) > 20 {
			details = details[:20]
		}
		is.Status.FailedImageDetails = details

		readyStatus := metav1.ConditionTrue
		readyReason := "Collected"
		readyMsg := fmt.Sprintf("Collected %d images (%d mirrored, %d pending, %d failed)", isTotal, isMirrored, isPending, isFailed)
		if isTotal == 0 {
			readyStatus = metav1.ConditionFalse
			readyReason = "Empty"
			readyMsg = "no images resolved yet"
		}
		setReadyCondition(&is.Status.Conditions, readyStatus, readyReason, readyMsg, is.Generation)

		if err := m.Client.Status().Update(ctx, is); err != nil {
			fmt.Printf("Failed to update ImageSet %s status: %v\n", is.Name, err)
		}
	}
}

func setReadyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string, gen int64) {
	if conditions == nil {
		return
	}
	for i, c := range *conditions {
		if c.Type == "Ready" {
			if c.Status != status || c.Reason != reason || c.Message != message || c.ObservedGeneration != gen {
				(*conditions)[i].Status = status
				(*conditions)[i].Reason = reason
				(*conditions)[i].Message = message
				(*conditions)[i].ObservedGeneration = gen
				(*conditions)[i].LastTransitionTime = metav1.Now()
			}
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               "Ready",
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

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	mounts = append(mounts, corev1.VolumeMount{
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
		mounts = append(mounts, corev1.VolumeMount{
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

	envVars = append(envVars, workerProxyEnvVars(mt.Spec.Proxy)...)

	if mt.Spec.CABundle != nil {
		caKey := mt.Spec.CABundle.Key
		if caKey == "" {
			caKey = "ca-bundle.crt"
		}
		envVars = append(envVars, corev1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: "/run/secrets/ca/" + caKey,
		})
		mounts = append(mounts, corev1.VolumeMount{
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
					VolumeMounts: mounts,
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

func resourcePtr(q string) *resource.Quantity {
	v := resource.MustParse(q)
	return &v
}

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

func workerBuildEffectiveNoProxy(userNoProxy string) string {
	base := strings.Join([]string{"localhost", "127.0.0.1", ".svc", ".svc.cluster.local"}, ",")
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
