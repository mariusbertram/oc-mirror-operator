package manager

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func BenchmarkCleanupFinishedWorkers(b *testing.B) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	for _, count := range []int{10, 50, 100} {
		b.Run(fmt.Sprintf("Pods_%d", count), func(b *testing.B) {
			cs := k8sfake.NewSimpleClientset()
			m := &MirrorManager{
				Client:     c,
				Clientset:  cs,
				Namespace:  "default",
				TargetName: "test-target",
				inProgress: make(map[string]string),
			}

			// Pre-populate Fake client with pods
			for i := 0; i < count; i++ {
				podName := fmt.Sprintf("worker-pod-%d", i)
				dest := fmt.Sprintf("dest-%d", i)

				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: "default",
						Labels: map[string]string{
							"app":          "oc-mirror-worker",
							"mirrortarget": m.TargetName,
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				}
				_, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
				if err != nil {
					b.Fatalf("Failed to create pod: %v", err)
				}

				m.inProgress[dest] = podName
			}

			ctx := context.Background()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Re-add to inProgress mapping if it was modified
				for j := 0; j < count; j++ {
					podName := fmt.Sprintf("worker-pod-%d", j)
					dest := fmt.Sprintf("dest-%d", j)
					m.inProgress[dest] = podName
				}

				m.cleanupFinishedWorkers(ctx)
			}
		})
	}
}
