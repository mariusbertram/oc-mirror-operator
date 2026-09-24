package manager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// A honored recollect gives failed images — including permanently failed
// ones — a fresh retry cycle (#135).
func TestResetFailedForRecollectLocked(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{
		"perm":     {State: stateFailed, RetryCount: 10, PermanentlyFailed: true, LastError: "unauthorized"},
		"failed":   {State: stateFailed, RetryCount: 3, LastError: "timeout"},
		"mirrored": {State: stateMirrored},
		"busy":     {State: stateFailed, RetryCount: 2},
		"other-is": {State: stateFailed, RetryCount: 10, PermanentlyFailed: true},
	}
	m.owners = map[string][]string{
		"perm": {"my-is"}, "failed": {"my-is"}, "mirrored": {"my-is"}, "busy": {"my-is"}, "other-is": {"other"},
	}
	m.inProgress["busy"] = "worker-1"

	if !m.resetFailedForRecollectLocked("my-is") {
		t.Fatal("expected changes")
	}

	for _, dest := range []string{"perm", "failed"} {
		e := m.imageState[dest]
		if e.State != statePending || e.RetryCount != 0 || e.LastError != "" {
			t.Errorf("%s: got state=%q retry=%d err=%q, want a fresh Pending entry", dest, e.State, e.RetryCount, e.LastError)
		}
	}
	if !m.imageState["perm"].PermanentlyFailed {
		t.Error("PermanentlyFailed is a sticky history marker and must stay set")
	}
	if m.imageState["mirrored"].State != stateMirrored {
		t.Error("recollect must not touch Mirrored images")
	}
	if m.imageState["busy"].State != stateFailed {
		t.Error("entries with an in-flight worker must be left alone")
	}
	if m.imageState["other-is"].State != stateFailed {
		t.Error("entries of other ImageSets must be left alone")
	}
	if m.resetFailedForRecollectLocked("my-is") {
		t.Error("second call must report no changes")
	}
}

func patchTestManager(t *testing.T, annotations map[string]string) (*MirrorManager, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = mirrorv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "is", Namespace: "default", Annotations: annotations}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(is).Build()
	return NewWithClients(c, nil, "t", "default", "img", "", scheme), c
}

func getAnnotations(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "is"}, is); err != nil {
		t.Fatal(err)
	}
	return is.Annotations
}

func TestPatchImageSetAnnotations_ClearsHonoredRecollectAndRecordsMarker(t *testing.T) {
	m, c := patchTestManager(t, map[string]string{mirrorv1alpha1.RecollectAnnotation: "run-1"})
	resolvedFrom := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{
		Name: "is", Namespace: "default", Annotations: map[string]string{mirrorv1alpha1.RecollectAnnotation: "run-1"},
	}}

	desired := map[string]string{mirrorv1alpha1.RecollectHonoredAnnotation: "2026-09-24T00:00:00Z"}
	if err := m.patchImageSetAnnotations(context.Background(), resolvedFrom, desired); err != nil {
		t.Fatal(err)
	}

	got := getAnnotations(t, c)
	if _, ok := got[mirrorv1alpha1.RecollectAnnotation]; ok {
		t.Error("honored recollect annotation must be removed")
	}
	if got[mirrorv1alpha1.RecollectHonoredAnnotation] != "2026-09-24T00:00:00Z" {
		t.Errorf("recollect-honored marker = %q", got[mirrorv1alpha1.RecollectHonoredAnnotation])
	}
}

// A recollect requested while a resolve was running (new value) must survive
// the resolve's annotation patch so it is honored by the next resolve.
func TestPatchImageSetAnnotations_KeepsRecollectRequestedDuringResolve(t *testing.T) {
	for name, resolvedFrom := range map[string]map[string]string{
		"resolve without recollect":  nil,
		"resolve of older recollect": {mirrorv1alpha1.RecollectAnnotation: "run-1"},
	} {
		t.Run(name, func(t *testing.T) {
			m, c := patchTestManager(t, map[string]string{mirrorv1alpha1.RecollectAnnotation: "run-2"})
			is := &mirrorv1alpha1.ImageSet{ObjectMeta: metav1.ObjectMeta{Name: "is", Namespace: "default", Annotations: resolvedFrom}}

			if err := m.patchImageSetAnnotations(context.Background(), is, map[string]string{}); err != nil {
				t.Fatal(err)
			}

			if got := getAnnotations(t, c)[mirrorv1alpha1.RecollectAnnotation]; got != "run-2" {
				t.Errorf("recollect annotation = %q, want run-2 kept", got)
			}
		})
	}
}
