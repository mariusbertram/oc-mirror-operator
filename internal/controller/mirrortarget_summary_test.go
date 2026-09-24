package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func newSummaryTestReconciler(t *testing.T, summary *imagestate.Summary) (*MirrorTargetReconciler, *mirrorv1alpha1.MirrorTarget) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = mirrorv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt", Namespace: "ns"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{ImageSets: []string{"is-b", "is-a"}},
	}
	// Per-ImageSet counters double-count a shared image: 3 + 2 = 5.
	isA := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is-a", Namespace: "ns"},
		Status:     mirrorv1alpha1.ImageSetStatus{TotalImages: 3, MirroredImages: 3},
	}
	isB := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is-b", Namespace: "ns"},
		Status:     mirrorv1alpha1.ImageSetStatus{TotalImages: 2, MirroredImages: 2},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mt, isA, isB).Build()
	if summary != nil {
		if err := imagestate.SaveSummary(context.Background(), c, "ns", "mt", *summary, nil, nil); err != nil {
			t.Fatalf("SaveSummary: %v", err)
		}
	}
	return &MirrorTargetReconciler{Client: c, Scheme: scheme}, mt
}

func TestAggregateImageSetStatus_UsesManagerSummary(t *testing.T) {
	r, mt := newSummaryTestReconciler(t, &imagestate.Summary{
		ImageSets: []string{"is-a", "is-b"}, Total: 4, Mirrored: 3, Pending: 1,
	})
	if err := r.aggregateImageSetStatus(context.Background(), mt); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if mt.Status.TotalImages != 4 || mt.Status.MirroredImages != 3 || mt.Status.PendingImages != 1 || mt.Status.FailedImages != 0 {
		t.Fatalf("status = %+v", mt.Status)
	}
	if len(mt.Status.ImageSetStatuses) != 2 || mt.Status.ImageSetStatuses[0].Total != 3 {
		t.Fatalf("per-ImageSet breakdown = %+v", mt.Status.ImageSetStatuses)
	}
}

// A summary computed over a different set of ImageSets (spec.imageSets just
// edited, manager not caught up) is ignored in favour of the full computation.
func TestAggregateImageSetStatus_IgnoresSummaryForOtherImageSets(t *testing.T) {
	r, mt := newSummaryTestReconciler(t, &imagestate.Summary{
		ImageSets: []string{"is-a"}, Total: 99, Mirrored: 99,
	})
	if err := r.aggregateImageSetStatus(context.Background(), mt); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	// No state ConfigMaps: falls back to summing per-ImageSet counters.
	if mt.Status.TotalImages != 5 || mt.Status.MirroredImages != 5 {
		t.Fatalf("status = %+v", mt.Status)
	}
}

func TestImageSetCountsChangedPredicate(t *testing.T) {
	base := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "is", Namespace: "ns"},
		Status:     mirrorv1alpha1.ImageSetStatus{TotalImages: 2, MirroredImages: 1, PendingImages: 1},
	}
	annotated := base.DeepCopy()
	annotated.Annotations = map[string]string{"x": "y"}
	annotated.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	progressed := base.DeepCopy()
	progressed.Status.MirroredImages, progressed.Status.PendingImages = 2, 0

	tests := []struct {
		name string
		new  *mirrorv1alpha1.ImageSet
		want bool
	}{
		{"annotation and condition churn", annotated, false},
		{"mirrored count changed", progressed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageSetCountsChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tt.new}); got != tt.want {
				t.Fatalf("Update = %v, want %v", got, tt.want)
			}
		})
	}
	if !imageSetCountsChanged.Create(event.CreateEvent{Object: base}) {
		t.Error("Create must pass")
	}
	if !imageSetCountsChanged.Delete(event.DeleteEvent{Object: base}) {
		t.Error("Delete must pass")
	}
	if !imageSetCountsChanged.Update(event.UpdateEvent{ObjectOld: &corev1.ConfigMap{}, ObjectNew: base}) {
		t.Error("unexpected types must pass")
	}
}
