package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
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
	Client       client.Client
	Clientset    kubernetes.Interface
	TargetName   string
	Namespace    string
	Scheme       *runtime.Scheme
	Image        string
	mirrorClient *mirrorclient.MirrorClient
	authConfigPath string // path to Docker config for creating fresh clients

	workerToken string

	// State in memory — protected by mu
	mu              sync.RWMutex
	inProgress      map[string]string                      // dest → podName
	mirrored        map[string]bool                        // dest → true once successfully mirrored
	imageStates     map[string]imagestate.ImageState       // imageSetName → ImageState
	destToIS        map[string]string                      // dest → imageSetName (reverse lookup)
	lastDriftCheck  time.Time                              // last time we verified all mirrored images
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

	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)

	return &MirrorManager{
		Client:      c,
		Clientset:   cs,
		TargetName:  targetName,
		Namespace:   namespace,
		Scheme:      scheme,
		Image:       image,
		mirrorClient: mc,
		authConfigPath: authConfigPath,
		workerToken: hex.EncodeToString(tokenBytes),
		inProgress:  make(map[string]string),
		mirrored:    make(map[string]bool),
		imageStates: make(map[string]imagestate.ImageState),
		destToIS:    make(map[string]string),
	}
}

func (m *MirrorManager) Run(ctx context.Context) error {
	fmt.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

	// Rebuild in-progress state from any worker pods that survived a manager restart.
	if err := m.syncInProgressFromPods(ctx); err != nil {
		fmt.Printf("Warning: could not sync in-progress state from pods: %v\n", err)
	}

	// Start Status API Server (internal, port 8080)
	go m.runStatusAPI(ctx)

	// Start Resource Server (public, port 8081)
	go resources.NewServer(m.Client, m.Namespace, m.TargetName).Run(ctx)

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

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Status API server failed: %v\n", err)
		}
	}()

	<-ctx.Done()
	_ = server.Shutdown(context.Background())
}

func (m *MirrorManager) handleStatusUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer "+m.workerToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req WorkerStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Printf("Received status update from %s for %s\n", req.PodName, req.Destination)

	if req.Error != "" {
		m.setImageStateLocked(req.Destination, "Failed", req.Error)
	} else {
		m.mirrored[req.Destination] = true
		m.setImageStateLocked(req.Destination, "Mirrored", "")
	}

	// Remove from in-progress tracking. The pod itself is cleaned up by
	// cleanupFinishedWorkers() once it reaches Succeeded/Failed, so that
	// other batch items in the same pod can continue reporting.
	delete(m.inProgress, req.Destination)

	w.WriteHeader(http.StatusOK)
}

// cleanupFinishedWorkers removes completed/failed worker pods.
// First it handles tracked pods in m.inProgress (resetting Failed images to
// Pending), then it sweeps for any orphaned finished pods that fell out of
// tracking (e.g. due to a manager restart).
// Caller must NOT hold m.mu.
func (m *MirrorManager) cleanupFinishedWorkers(ctx context.Context) {
	m.mu.Lock()

	// 1. Tracked pods in m.inProgress.
	deletedPods := map[string]struct{}{}
	for dest, podName := range m.inProgress {
		pod, err := m.Clientset.CoreV1().Pods(m.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			delete(m.inProgress, dest)
			continue
		}
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		delete(m.inProgress, dest)
		if pod.Status.Phase == corev1.PodFailed {
			fmt.Printf("Worker pod %s for %s failed without reporting; resetting to Pending\n", podName, dest)
			m.setImageStateLocked(dest, "Pending", "")
		}
		if _, already := deletedPods[podName]; !already {
			_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
			deletedPods[podName] = struct{}{}
		}
	}

	m.mu.Unlock()

	// 2. Sweep for any orphaned finished worker pods not in m.inProgress.
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
	err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt)
	if err != nil {
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

	for _, is := range imageSets.Items {
		// Only process ImageSets listed in this MirrorTarget's spec.imageSets.
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}

		// Load per-image state from ConfigMap (cache in m.imageStates to avoid
		// repeated reads; reloaded on every reconcile to pick up external changes).
		isState, loadErr := imagestate.Load(ctx, m.Client, m.Namespace, is.Name)
		if loadErr != nil {
			fmt.Printf("Warning: failed to load image state for %s: %v\n", is.Name, loadErr)
			isState = make(imagestate.ImageState)
		}
		m.imageStates[is.Name] = isState

		// Periodic drift detection: clear the in-memory mirrored cache every 5
		// minutes so the loop below re-verifies that mirrored images still exist
		// in the target registry. This catches manual deletions or registry wipes.
		const driftCheckInterval = 5 * time.Minute
		if time.Since(m.lastDriftCheck) > driftCheckInterval {
			m.mirrored = make(map[string]bool)
			m.lastDriftCheck = time.Now()
			// Create a fresh regclient for drift checks to avoid auth scope
			// accumulation. Quay's nginx proxy returns 400 when the Bearer
			// token (which grows with each new repository scope) exceeds ~8 KB.
			m.mirrorClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
			fmt.Println("Drift detection: clearing mirrored cache for re-verification")
		}

		stateChanged := false
		var pendingImages []BatchItem
		checkClient := mirrorclient.NewMirrorClient(nil, m.authConfigPath)
		checkCount := 0

		for dest, entry := range isState {
			m.destToIS[dest] = is.Name

			// For images marked Mirrored in the ConfigMap but not yet verified
			// in memory: check the registry to confirm they still exist.
			if entry.State == "Mirrored" && !m.mirrored[dest] {
				// Refresh the client every 20 checks to prevent auth token
				// scope accumulation (Quay's nginx rejects tokens > ~8 KB).
				checkCount++
				if checkCount%20 == 0 {
					checkClient = mirrorclient.NewMirrorClient(nil, m.authConfigPath)
				}
				exists, checkErr := checkClient.CheckExist(ctx, dest)
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
				entry.State = "Pending"
				entry.LastError = ""
				entry.RetryCount = 0
				stateChanged = true
			}

			if m.mirrored[dest] {
				if entry.State != "Mirrored" {
					entry.State = "Mirrored"
					stateChanged = true
				}
				continue
			}

			if entry.State == "Failed" {
				if entry.RetryCount < 10 {
					entry.State = "Pending"
					// Don't increment here; retryCount was already incremented
					// when the failure was recorded in setImageStateLocked.
					stateChanged = true
				}
				continue
			}

			if entry.State != "Pending" {
				continue
			}
			if m.inProgress[dest] != "" {
				continue
			}

			pendingImages = append(pendingImages, BatchItem{Source: entry.Source, Dest: dest})
		}

		// Dispatch batches up to concurrency limit (counted as distinct pods).
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
				m.destToIS[item.Dest] = is.Name
			}
			activePods[podName] = struct{}{}
			stateChanged = true
			fmt.Printf("Started worker pod %s for batch of %d images\n", podName, len(batch))
		}

		// Flush state changes to ConfigMap and update ImageSet status counts.
		if stateChanged {
			if err := imagestate.Save(ctx, m.Client, m.Namespace, &is, isState); err != nil {
				fmt.Printf("Warning: failed to save image state for %s: %v\n", is.Name, err)
			}
		}
		m.updateImageSetStatusLocked(ctx, &is, isState)
	}

	return nil
}

// setImageStateLocked updates the in-memory state for a single destination.
// Caller must hold m.mu. The ConfigMap is NOT flushed here; the next reconcile
// tick (every 30s) will persist the change. This keeps the status-update HTTP
// handler fast.
func (m *MirrorManager) setImageStateLocked(dest, st, lastError string) {
	isName, ok := m.destToIS[dest]
	if !ok {
		return
	}
	isState, ok := m.imageStates[isName]
	if !ok {
		return
	}
	entry, ok := isState[dest]
	if !ok {
		return
	}
	entry.State = st
	entry.LastError = lastError
	if st == "Failed" {
		entry.RetryCount++
	}
}

// updateImageSetStatusLocked updates the ImageSet status with aggregate counts.
// Caller must hold m.mu.
func (m *MirrorManager) updateImageSetStatusLocked(ctx context.Context, is *mirrorv1alpha1.ImageSet, state imagestate.ImageState) {
	total, mirrored, pending, failed := imagestate.Counts(state)
	is.Status.TotalImages = total
	is.Status.MirroredImages = mirrored
	is.Status.PendingImages = pending
	is.Status.FailedImages = failed
	if err := m.Client.Status().Update(ctx, is); err != nil {
		fmt.Printf("Failed to update ImageSet %s status: %v\n", is.Name, err)
	}
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
			Name:  "WORKER_TOKEN",
			Value: m.workerToken,
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

	// Ephemeral volume for buffering large blobs to disk before upload.
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "blob-buffer",
		MountPath: "/tmp/blob-buffer",
	})
	volumes = append(volumes, corev1.Volume{
		Name: "blob-buffer",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

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
						{Key: "config.json", Path: "config.json"},
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
			ServiceAccountName: "oc-mirror-worker",
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

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
