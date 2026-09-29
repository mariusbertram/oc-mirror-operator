package manager

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// A resolve must be recorded in the ImageSet status in the same tick, even
// when no worker callback made the status dirty: before, ObservedGeneration
// and Ready stayed as they were (e.g. the controller's "Unbound" from before
// the MirrorTarget referenced the ImageSet) and the ImageSet was re-resolved
// on every tick (#189).
func TestReconcile_RecordsResolveInStatus(t *testing.T) {
	m, _ := newReconcileTestManager(t, []string{"my-is"}, "my-is")
	ctx := context.Background()
	key := client.ObjectKey{Name: "my-is", Namespace: "default"}

	is := &mirrorv1alpha1.ImageSet{}
	if err := m.Client.Get(ctx, key, is); err != nil {
		t.Fatal(err)
	}
	// Spec edited since the last resolve, and still carrying the controller's
	// condition from before the MirrorTarget referenced it.
	is.Generation = 2
	if err := m.Client.Update(ctx, is); err != nil {
		t.Fatal(err)
	}
	is.Status.Conditions = []metav1.Condition{{
		Type: conditionReady, Status: metav1.ConditionFalse, Reason: "Unbound",
		Message: "no MirrorTarget references ImageSet my-is", ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	if err := m.Client.Status().Update(ctx, is); err != nil {
		t.Fatal(err)
	}

	if err := m.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	got := &mirrorv1alpha1.ImageSet{}
	if err := m.Client.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("ObservedGeneration = %d, want %d (the resolve was not recorded)", got.Status.ObservedGeneration, got.Generation)
	}
	if got.Status.LastSuccessfulPollTime == nil {
		t.Error("expected LastSuccessfulPollTime to be set by the resolve")
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionReady)
	if ready == nil || ready.Reason == "Unbound" || ready.ObservedGeneration != got.Generation {
		t.Errorf("Ready = %+v, want the manager's condition for generation %d", ready, got.Generation)
	}
}
