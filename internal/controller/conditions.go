package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setCondition merges a new condition into the slice keyed by Type. It updates
// Status, Reason, and Message in place, bumping LastTransitionTime only when
// Status actually flips. ObservedGeneration is set so consumers can tell which
// spec generation the condition was last evaluated against.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, observedGeneration ...int64) {
	now := metav1.Now()
	var gen int64
	if len(observedGeneration) > 0 {
		gen = observedGeneration[0]
	}
	for i, c := range *conditions {
		if c.Type == condType {
			if c.Status != status || c.Reason != reason || c.Message != message || c.ObservedGeneration != gen {
				(*conditions)[i].Status = status
				(*conditions)[i].Reason = reason
				(*conditions)[i].Message = message
				(*conditions)[i].ObservedGeneration = gen
				if c.Status != status {
					(*conditions)[i].LastTransitionTime = now
				}
			}
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
		LastTransitionTime: now,
	})
}
