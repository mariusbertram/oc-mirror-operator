package imagestate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSummary_SaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if _, ok, err := LoadSummary(ctx, c, "ns", "mt"); err != nil || ok {
		t.Fatalf("missing summary: ok=%v err=%v", ok, err)
	}

	want := Summary{ImageSets: []string{"a", "b"}, Total: 5, Mirrored: 3, Pending: 1, Failed: 1}
	if err := SaveSummary(ctx, c, "ns", "mt", want, nil, nil); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	got, ok, err := LoadSummary(ctx, c, "ns", "mt")
	if err != nil || !ok {
		t.Fatalf("LoadSummary: ok=%v err=%v", ok, err)
	}
	if got.Total != 5 || got.Mirrored != 3 || got.Pending != 1 || got.Failed != 1 || len(got.ImageSets) != 2 || got.ImageSets[1] != "b" {
		t.Fatalf("got %+v", got)
	}

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: "ns", Name: SummaryConfigMapName("mt")}
	_ = c.Get(ctx, key, cm)
	rv := cm.ResourceVersion

	// Unchanged content is not rewritten.
	if err := SaveSummary(ctx, c, "ns", "mt", want, nil, nil); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	_ = c.Get(ctx, key, cm)
	if cm.ResourceVersion != rv {
		t.Fatal("unchanged summary was rewritten")
	}

	want.Mirrored, want.Pending = 4, 0
	want.ImageSets = nil
	if err := SaveSummary(ctx, c, "ns", "mt", want, nil, nil); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	got, _, _ = LoadSummary(ctx, c, "ns", "mt")
	if got.Mirrored != 4 || got.Pending != 0 || got.ImageSets != nil {
		t.Fatalf("after update got %+v", got)
	}
}

func TestSummary_SetsOwnerOnCreate(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "uid-1"}}
	if err := SaveSummary(ctx, c, "ns", "mt", Summary{}, owner, scheme); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: SummaryConfigMapName("mt")}, cm)
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "uid-1" {
		t.Fatalf("owner refs = %+v", cm.OwnerReferences)
	}
}

func TestSummary_UnparsableIsNotOK(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: SummaryConfigMapName("mt"), Namespace: "ns"},
		Data:       map[string]string{"total": "x"},
	}).Build()
	if _, ok, err := LoadSummary(ctx, c, "ns", "mt"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestSummary_GetErrors(t *testing.T) {
	ctx := context.Background()
	// Scheme without corev1: every ConfigMap Get fails.
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if _, _, err := LoadSummary(ctx, c, "ns", "mt"); err == nil {
		t.Fatal("LoadSummary: expected error")
	}
	if err := SaveSummary(ctx, c, "ns", "mt", Summary{}, nil, nil); err == nil {
		t.Fatal("SaveSummary: expected error")
	}
}
