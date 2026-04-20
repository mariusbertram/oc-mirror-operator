package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
)

type WorkerStatusRequest struct {
	PodName     string `json:"podName"`
	Destination string `json:"destination"`
	Digest      string `json:"digest"`
	Error       string `json:"error,omitempty"`
}

type MirrorManager struct {
	Client       client.Client
	Clientset    kubernetes.Interface
	TargetName   string
	Namespace    string
	Scheme       *runtime.Scheme
	Image        string
	StateManager *state.StateManager
	mirrorClient *mirrorclient.MirrorClient

	workerToken string

	// State in memory
	mu         sync.RWMutex
	inProgress map[string]string // imageDestination -> podName
	mirrored   map[string]bool   // imageDestination -> true once successfully mirrored
	meta       *state.Metadata
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
		Client:       c,
		Clientset:    cs,
		TargetName:   targetName,
		Namespace:    namespace,
		Scheme:       scheme,
		Image:        image,
		StateManager: state.New(mc),
		mirrorClient: mc,
		workerToken:  hex.EncodeToString(tokenBytes),
		inProgress:   make(map[string]string),
		mirrored:     make(map[string]bool),
	}
}

func (m *MirrorManager) Run(ctx context.Context) error {
	fmt.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

	// Rebuild in-progress state from any worker pods that survived a manager restart.
	if err := m.syncInProgressFromPods(ctx); err != nil {
		fmt.Printf("Warning: could not sync in-progress state from pods: %v\n", err)
	}

	// Start Status API Server
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
			continue
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
		m.updateImageStatus(context.Background(), req.Destination, "Failed", req.Error)
	} else {
		if m.meta != nil {
			m.meta.MirroredImages[req.Destination] = req.Digest
		}
		m.mirrored[req.Destination] = true
		m.updateImageStatus(context.Background(), req.Destination, "Mirrored", "")
	}

	// Clean up pod
	delete(m.inProgress, req.Destination)
	_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(context.Background(), req.PodName, metav1.DeleteOptions{})

	w.WriteHeader(http.StatusOK)
}

func (m *MirrorManager) reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mt := &mirrorv1alpha1.MirrorTarget{}
	err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt)
	if err != nil {
		return err
	}

	// Build a mirror client that honours the target's insecure setting.
	stateManager := m.StateManager
	if mt.Spec.Insecure {
		host := mt.Spec.Registry
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		mc := mirrorclient.NewMirrorClient([]string{host}, "")
		stateManager = state.New(mc)
	}

	// 1. Load Metadata from target registry
	metaRepo := fmt.Sprintf("%s/oc-mirror-metadata", mt.Spec.Registry)
	meta, _, err := stateManager.ReadMetadata(ctx, metaRepo, "latest")
	if err != nil {
		fmt.Printf("Warning: Failed to read metadata from %s: %v. Initializing new state.\n", metaRepo, err)
		meta = &state.Metadata{MirroredImages: make(map[string]string)}
	}
	m.meta = meta
	// Sync in-memory mirrored set from persisted metadata (survives manager restarts).
	for dest := range meta.MirroredImages {
		m.mirrored[dest] = true
	}

	imageSets := &mirrorv1alpha1.ImageSetList{}
	if err := m.Client.List(ctx, imageSets, client.InNamespace(m.Namespace)); err != nil {
		return err
	}

	// 2. Process ImageSets
	hasChanged := false
	for _, is := range imageSets.Items {
		if is.Spec.TargetRef != m.TargetName {
			continue
		}

		for i := range is.Status.TargetImages {
			img := &is.Status.TargetImages[i]

			// For images already marked Mirrored in k8s status: verify they actually exist
			// in the target registry. If the manager restarted and in-memory state was lost,
			// re-check via the registry API before trusting the stale k8s status.
			if img.State == "Mirrored" && !m.mirrored[img.Destination] {
				exists, checkErr := m.mirrorClient.CheckExist(ctx, img.Destination)
				if checkErr != nil {
					fmt.Printf("CheckExist error for %s: %v – will retry mirror\n", img.Destination, checkErr)
				}
				if exists {
					m.mirrored[img.Destination] = true
					continue
				}
				// Not actually in registry — reset so it gets re-mirrored.
				fmt.Printf("Image %s marked Mirrored but not found in registry; resetting to Pending\n", img.Destination)
				img.State = "Pending"
				img.LastError = ""
				hasChanged = true
				_ = m.Client.Status().Update(ctx, &is)
			}

			// Skip if already mirrored (in-memory or persisted metadata)
			if m.mirrored[img.Destination] || m.meta.MirroredImages[img.Destination] != "" {
				if img.State != "Mirrored" {
					img.State = "Mirrored"
					_ = m.Client.Status().Update(ctx, &is)
				}
				continue
			}

			if img.State == "Failed" {
				retryCount := parseRetryCount(img.LastError)
				if retryCount < 3 {
					img.State = "Pending"
					img.LastError = incrementRetryCount(img.LastError, retryCount)
					_ = m.Client.Status().Update(ctx, &is)
				}
				continue
			}

			if img.State != "Pending" {
				continue
			}

			if m.inProgress[img.Destination] != "" {
				continue
			}

			// Honour the configured concurrency limit (default 20).
			concurrency := mt.Spec.Concurrency
			if concurrency <= 0 {
				concurrency = 20
			}
			if len(m.inProgress) >= concurrency {
				break
			}

			// Start a worker pod
			podName, err := m.startWorker(ctx, mt, img.Source, img.Destination)
			if err != nil {
				fmt.Printf("Failed to start worker for %s: %v\n", img.Destination, err)
				continue
			}
			m.inProgress[img.Destination] = podName
			fmt.Printf("Started worker pod %s for image %s\n", podName, img.Destination)
			hasChanged = true
		}
	}

	// 3. Save metadata if anything changed
	if hasChanged {
		_, err = stateManager.WriteMetadata(ctx, metaRepo, "latest", m.meta)
		if err != nil {
			fmt.Printf("Error writing metadata: %v\n", err)
		}
	}

	return nil
}

func (m *MirrorManager) updateImageStatus(ctx context.Context, dest, state, lastError string) {
	imageSets := &mirrorv1alpha1.ImageSetList{}
	if err := m.Client.List(ctx, imageSets, client.InNamespace(m.Namespace)); err != nil {
		return
	}

	for _, is := range imageSets.Items {
		changed := false
		for i := range is.Status.TargetImages {
			if is.Status.TargetImages[i].Destination == dest {
				is.Status.TargetImages[i].State = state
				is.Status.TargetImages[i].LastError = lastError
				if state == "Mirrored" {
					is.Status.MirroredImages++
				}
				changed = true
			}
		}
		if changed {
			if err := m.Client.Status().Update(ctx, &is); err != nil {
				fmt.Printf("Failed to update ImageSet %s status: %v\n", is.Name, err)
			}
		}
	}
}

func (m *MirrorManager) startWorker(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, src, dest string) (string, error) {
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
	}

	containerArgs := []string{
		"worker",
		"--src", src,
		"--dest", dest,
	}
	if mt.Spec.Insecure {
		containerArgs = append(containerArgs, "--insecure")
	}

	var volumeMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	// Only mount auth secret if one is configured
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
				"mirror.openshift.io/destination": dest,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: "oc-mirror-worker",
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

func parseRetryCount(lastError string) int {
	if strings.HasPrefix(lastError, "retry:") {
		parts := strings.SplitN(lastError, ":", 3)
		if len(parts) >= 2 {
			n, _ := strconv.Atoi(parts[1])
			return n
		}
	}
	return 0
}

func incrementRetryCount(lastError string, currentCount int) string {
	msg := lastError
	if strings.HasPrefix(msg, "retry:") {
		parts := strings.SplitN(msg, ":", 3)
		if len(parts) >= 3 {
			msg = parts[2]
		}
	}
	return fmt.Sprintf("retry:%d:%s", currentCount+1, msg)
}

func pointerTo[T any](v T) *T {
	return &v
}
