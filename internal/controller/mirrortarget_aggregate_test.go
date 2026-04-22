package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func TestAggregateImageSetStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := mirrorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "quay-internal", Namespace: "ocp-mirror-system"},
		Spec: mirrorv1alpha1.MirrorTargetSpec{
			ImageSets: []string{"is-b", "is-a", "is-missing"},
		},
	}
	isA := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is-a", Namespace: "ocp-mirror-system"},
		Status:     mirrorv1alpha1.ImageSetStatus{TotalImages: 100, MirroredImages: 60, PendingImages: 30, FailedImages: 10},
	}
	isB := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is-b", Namespace: "ocp-mirror-system"},
		Status:     mirrorv1alpha1.ImageSetStatus{TotalImages: 50, MirroredImages: 50, PendingImages: 0, FailedImages: 0},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(mt, isA, isB).Build()
	r := &MirrorTargetReconciler{Client: c, Scheme: scheme}

	if err := r.aggregateImageSetStatus(context.Background(), mt); err != nil {
		t.Fatalf("aggregateImageSetStatus: %v", err)
	}

	if got, want := mt.Status.TotalImages, 150; got != want {
		t.Errorf("TotalImages: got %d want %d", got, want)
	}
	if got, want := mt.Status.MirroredImages, 110; got != want {
		t.Errorf("MirroredImages: got %d want %d", got, want)
	}
	if got, want := mt.Status.PendingImages, 30; got != want {
		t.Errorf("PendingImages: got %d want %d", got, want)
	}
	if got, want := mt.Status.FailedImages, 10; got != want {
		t.Errorf("FailedImages: got %d want %d", got, want)
	}

	if len(mt.Status.ImageSetStatuses) != 3 {
		t.Fatalf("expected 3 summaries, got %d", len(mt.Status.ImageSetStatuses))
	}
	// Sorted alphabetically: is-a, is-b, is-missing
	names := []string{
		mt.Status.ImageSetStatuses[0].Name,
		mt.Status.ImageSetStatuses[1].Name,
		mt.Status.ImageSetStatuses[2].Name,
	}
	if names[0] != "is-a" || names[1] != "is-b" || names[2] != "is-missing" {
		t.Fatalf("expected alphabetical order [is-a, is-b, is-missing], got %v", names)
	}
	if !mt.Status.ImageSetStatuses[0].Found || !mt.Status.ImageSetStatuses[1].Found {
		t.Errorf("existing ImageSets must be Found=true")
	}
	if mt.Status.ImageSetStatuses[2].Found {
		t.Errorf("missing ImageSet must be Found=false")
	}
	if mt.Status.ImageSetStatuses[2].Total != 0 {
		t.Errorf("missing ImageSet must contribute zero counters")
	}
	if mt.Status.ImageSetStatuses[0].Total != 100 || mt.Status.ImageSetStatuses[0].Failed != 10 {
		t.Errorf("is-a summary mismatch: %+v", mt.Status.ImageSetStatuses[0])
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
