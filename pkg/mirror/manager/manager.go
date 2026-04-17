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

	workerToken string

	// State in memory
	mu         sync.RWMutex
	inProgress map[string]string // imageDestination -> podName
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

	image := os.Getenv("CONTROLLER_IMAGE")
	if image == "" {
		return nil, fmt.Errorf("CONTROLLER_IMAGE environment variable is required but not set")
	}

	return NewWithClients(c, cs, targetName, namespace, image, scheme), nil
}

func NewWithClients(c client.Client, cs kubernetes.Interface, targetName, namespace, image string, scheme *runtime.Scheme) *MirrorManager {
	mc := mirrorclient.NewMirrorClient(nil, "")

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
		workerToken:  hex.EncodeToString(tokenBytes),
		inProgress:   make(map[string]string),
	}
}

func (m *MirrorManager) Run(ctx context.Context) error {
	fmt.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

	// Start Status API Server
	go m.runStatusAPI(ctx)

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

	// 1. Load Metadata from target registry
	metaRepo := fmt.Sprintf("%s/oc-mirror-metadata", mt.Spec.Registry)
	meta, _, err := m.StateManager.ReadMetadata(ctx, metaRepo, "latest")
	if err != nil {
		fmt.Printf("Warning: Failed to read metadata from %s: %v. Initializing new state.\n", metaRepo, err)
		meta = &state.Metadata{MirroredImages: make(map[string]string)}
	}
	m.meta = meta

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

			// Skip if already in metadata
			if m.meta.MirroredImages[img.Destination] != "" {
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

			// Check if we have too many workers
			if len(m.inProgress) >= 10 {
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
		_, err = m.StateManager.WriteMetadata(ctx, metaRepo, "latest", m.meta)
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-worker-", mt.Name),
			Namespace:    m.Namespace,
			Labels: map[string]string{
				"app":          "oc-mirror-worker",
				"mirrortarget": m.TargetName,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
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
					Args: []string{
						"worker",
						"--src", src,
						"--dest", dest,
					},
					Env: []corev1.EnvVar{
						{
							Name:  "DOCKER_CONFIG",
							Value: "/run/secrets/dockerconfig",
						},
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
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "dockerconfig",
							MountPath: "/run/secrets/dockerconfig",
							ReadOnly:  true,
						},
					},
					Resources: mt.Spec.Worker.Resources,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "dockerconfig",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: mt.Spec.AuthSecret,
							Items: []corev1.KeyToPath{
								{
									Key:  ".dockerconfigjson",
									Path: "config.json",
								},
							},
						},
					},
				},
			},
			NodeSelector: mt.Spec.Worker.NodeSelector,
			Tolerations:  mt.Spec.Worker.Tolerations,
		},
	}

	if mt.Spec.Insecure {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "--insecure")
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
