package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/state"
)

type MirrorManager struct {
	Client       client.Client
	Clientset    kubernetes.Interface
	TargetName   string
	Namespace    string
	Scheme       *runtime.Scheme
	Image        string
	StateManager *state.StateManager

	// State in memory
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

	return NewWithClients(c, cs, targetName, namespace, scheme), nil
}

func NewWithClients(c client.Client, cs kubernetes.Interface, targetName, namespace string, scheme *runtime.Scheme) *MirrorManager {
	image := os.Getenv("CONTROLLER_IMAGE")
	if image == "" {
		image = "controller:latest"
	}

	mc := mirrorclient.NewMirrorClient()

	return &MirrorManager{
		Client:       c,
		Clientset:    cs,
		TargetName:   targetName,
		Namespace:    namespace,
		Scheme:       scheme,
		Image:        image,
		StateManager: state.New(mc),
		inProgress:   make(map[string]string),
	}
}

func (m *MirrorManager) Run(ctx context.Context) error {
	fmt.Printf("Starting Mirror Manager for %s in namespace %s\n", m.TargetName, m.Namespace)

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

func (m *MirrorManager) reconcile(ctx context.Context) error {
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

	// 2. Clean up finished pods and update state
	if err := m.cleanupPods(ctx); err != nil {
		return err
	}

	// 3. Process ImageSets
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

	// 4. Save metadata if anything changed
	if hasChanged || len(m.inProgress) == 0 {
		_, err = m.StateManager.WriteMetadata(ctx, metaRepo, "latest", m.meta)
		if err != nil {
			fmt.Printf("Error writing metadata: %v\n", err)
		}
	}

	return nil
}

func (m *MirrorManager) cleanupPods(ctx context.Context) error {
	for dest, podName := range m.inProgress {
		pod, err := m.Clientset.CoreV1().Pods(m.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				delete(m.inProgress, dest)
			}
			continue
		}

		if pod.Status.Phase == corev1.PodSucceeded {
			fmt.Printf("Worker pod %s succeeded for %s\n", podName, dest)

			// Extract digest from logs
			digest := m.getDigestFromLogs(ctx, podName)
			if digest == "" {
				digest = "unknown"
			}

			// Add to metadata
			m.meta.MirroredImages[dest] = digest
			m.updateImageStatus(ctx, dest, "Mirrored", "")
			delete(m.inProgress, dest)
			_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		} else if pod.Status.Phase == corev1.PodFailed {
			fmt.Printf("Worker pod %s failed for %s\n", podName, dest)
			m.updateImageStatus(ctx, dest, "Failed", "Pod failed")
			delete(m.inProgress, dest)
			_ = m.Clientset.CoreV1().Pods(m.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		}
	}
	return nil
}

func (m *MirrorManager) getDigestFromLogs(ctx context.Context, podName string) string {
	req := m.Clientset.CoreV1().Pods(m.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	logs, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer logs.Close()

	buf := new(strings.Builder)
	_, _ = io.Copy(buf, logs)

	logStr := buf.String()
	for _, line := range strings.Split(logStr, "\n") {
		if strings.Contains(line, "RESULT_DIGEST=") {
			parts := strings.Split(line, "RESULT_DIGEST=")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
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
			Containers: []corev1.Container{
				{
					Name:  "worker",
					Image: m.Image,
					Args: []string{
						"worker",
						"--src", src,
						"--dest", dest,
					},
					Resources: mt.Spec.Worker.Resources,
				},
			},
			NodeSelector: mt.Spec.Worker.NodeSelector,
			Tolerations:  mt.Spec.Worker.Tolerations,
		},
	}

	if mt.Spec.Insecure {
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "--insecure")
	}

	created, err := m.Clientset.CoreV1().Pods(m.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}
