package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition_UpdatesReason(t *testing.T) {
	conds := []metav1.Condition{}
	setCondition(&conds, "Ready", metav1.ConditionTrue, "Initial", "msg")
	if len(conds) != 1 || conds[0].Reason != "Initial" {
		t.Fatalf("expected initial condition with reason 'Initial', got %#v", conds)
	}

	// Same status+message but new reason → must update reason in place,
	// must NOT bump LastTransitionTime (status didn't change).
	originalLTT := conds[0].LastTransitionTime
	time.Sleep(10 * time.Millisecond)
	setCondition(&conds, "Ready", metav1.ConditionTrue, "Updated", "msg")
	if conds[0].Reason != "Updated" {
		t.Fatalf("expected reason to be updated to 'Updated', got %q", conds[0].Reason)
	}
	if !conds[0].LastTransitionTime.Equal(&originalLTT) {
		t.Fatalf("LastTransitionTime should not change when only reason changes")
	}

	// Status flip → LastTransitionTime must change.
	setCondition(&conds, "Ready", metav1.ConditionFalse, "Updated", "msg")
	if conds[0].Status != metav1.ConditionFalse {
		t.Fatalf("expected status to flip to False")
	}
	if conds[0].LastTransitionTime.Equal(&originalLTT) {
		t.Fatalf("LastTransitionTime should bump on status flip")
	}
}
