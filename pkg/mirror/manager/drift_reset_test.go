package manager

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// newReconcileTestManager returns a manager backed by fake clients holding a
// MirrorTarget "test" (polling disabled) that references specImageSets, plus
// an already-resolved ImageSet object for each name in imageSets.
func newReconcileTestManager(t *testing.T, specImageSets []string, imageSets ...string) (*MirrorManager, *k8sfake.Clientset) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = mirrorv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	objs := []runtime.Object{&mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: mirrorv1alpha1.MirrorTargetSpec{
			Registry:     "reg.io",
			ImageSets:    specImageSets,
			PollInterval: &metav1.Duration{Duration: -1},
		},
	}}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, name := range imageSets {
		is := &mirrorv1alpha1.ImageSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
			Status:     mirrorv1alpha1.ImageSetStatus{ObservedGeneration: 1},
		}
		objs = append(objs, is)
		b = b.WithStatusSubresource(is)
	}
	cs := k8sfake.NewSimpleClientset()
	m := NewWithClients(b.WithRuntimeObjects(objs...).Build(), cs, "test", "default", "img", "", scheme)
	// Keep the background drift sweep out of these tests.
	m.lastDriftCheck = time.Now()
	return m, cs
}

func countWorkerPods(t *testing.T, cs *k8sfake.Clientset) int {
	t.Helper()
	pods, err := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return len(pods.Items)
}

// An image the drift sweep finds missing from the target registry must be
// re-mirrored, not flipped back to Mirrored by the next tick (#129).
func TestDriftReset_MissingImageIsRedispatched(t *testing.T) {
	host := fakeExistenceServer(t, false)
	m, cs := newReconcileTestManager(t, []string{"my-is"}, "my-is")
	if _, err := m.clientCache.GetOrCreate([]string{host}, ""); err != nil {
		t.Fatal(err)
	}
	dest := host + "/repo:v1"
	m.imageState = imagestate.ImageState{dest: {Source: "quay.io/repo:v1", State: stateMirrored}}
	m.owners = map[string][]string{dest: {"my-is"}}

	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.checkDriftOne(context.Background(), dest, nil)
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := m.imageState[dest].State; got != statePending {
		t.Errorf("state = %q, want %q", got, statePending)
	}
	// The fake clientset does not honour GenerateName, so the pod name is
	// empty — only check that a pod was created and the dest is tracked.
	if _, tracked := m.inProgress[dest]; !tracked || countWorkerPods(t, cs) != 1 {
		t.Errorf("expected a worker to be dispatched for %s", dest)
	}
}

// A worker reporting a failure for a destination that was Mirrored before
// (e.g. re-mirror after drift) must stay Failed and be retried.
func TestSetImageStateLocked_ClearsMirroredFlag(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{"d": {State: stateMirrored}}
	m.mirrored["d"] = true

	m.setImageStateLocked("d", stateFailed, "boom")

	if m.mirrored["d"] {
		t.Error("expected mirrored flag to be cleared on a transition away from Mirrored")
	}
}
