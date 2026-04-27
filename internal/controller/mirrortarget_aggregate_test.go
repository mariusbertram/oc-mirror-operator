package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func TestAggregateImageSetStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = mirrorv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "quay-internal", Namespace: "oc-mirror-system"},
		Spec: mirrorv1alpha1.MirrorTargetSpec{
			ImageSets: []string{"is-a", "is-b"},
		},
	}

	// Create consolidated state in a ConfigMap.
	state := imagestate.ImageState{
		"img1": {Source: "s1", State: "Mirrored", Refs: []imagestate.ImageRef{{ImageSet: "is-a"}}},
		"img2": {Source: "s2", State: "Mirrored", Refs: []imagestate.ImageRef{{ImageSet: "is-b"}}},
		"img3": {Source: "s3", State: "Pending", Refs: []imagestate.ImageRef{{ImageSet: "is-a"}, {ImageSet: "is-b"}}},
	}

	// We need a real-ish client that can handle ConfigMaps.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt).Build()

	// Manually save the state for target.
	if err := imagestate.SaveForTarget(context.Background(), c, "oc-mirror-system", mt, state); err != nil {
		t.Fatalf("failed to save state: %v", err)
	}

	r := &MirrorTargetReconciler{Client: c, Scheme: scheme}

	if err := r.aggregateImageSetStatus(context.Background(), mt); err != nil {
		t.Fatalf("aggregateImageSetStatus: %v", err)
	}

	// mt-level counts are unique destinations.
	if got, want := mt.Status.TotalImages, 3; got != want {
		t.Errorf("TotalImages: got %d want %d", got, want)
	}
	if got, want := mt.Status.MirroredImages, 2; got != want {
		t.Errorf("MirroredImages: got %d want %d", got, want)
	}
	if got, want := mt.Status.PendingImages, 1; got != want {
		t.Errorf("PendingImages: got %d want %d", got, want)
	}

	if len(mt.Status.ImageSetStatuses) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(mt.Status.ImageSetStatuses))
	}
}

func TestMirrorTargetsForImageSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := mirrorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	is := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-is", Namespace: "ns1"},
	}
	mt1 := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt1", Namespace: "ns1"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"shared-is", "other"}},
	}
	mt2 := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt2", Namespace: "ns1"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"shared-is"}},
	}
	mtUnrelated := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt3", Namespace: "ns1"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"different-is"}},
	}
	mtOtherNS := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt4", Namespace: "ns2"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"shared-is"}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(is, mt1, mt2, mtUnrelated, mtOtherNS).Build()
	r := &MirrorTargetReconciler{Client: c, Scheme: scheme}

	reqs := r.mirrorTargetsForImageSet(context.Background(), is)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 enqueues (mt1+mt2), got %d: %+v", len(reqs), reqs)
	}
	got := map[string]bool{}
	for _, req := range reqs {
		if req.Namespace != "ns1" {
			t.Errorf("expected namespace ns1, got %s", req.Namespace)
		}
		got[req.Name] = true
	}
	if !got["mt1"] || !got["mt2"] {
		t.Errorf("expected mt1+mt2 enqueued, got %v", got)
	}
}
