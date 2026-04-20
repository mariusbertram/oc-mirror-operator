package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodConfig defines configuration for the manager or worker pods
type PodConfig struct {
	// Resources defines the compute resources required by this pod.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector is a selector which must be true for the pod to fit on a node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allows the pod to be scheduled onto nodes with matching taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// MirrorTargetSpec defines the desired state of MirrorTarget
type MirrorTargetSpec struct {
	// Registry is the URL of the target registry (e.g. registry.example.com/mirror)
	Registry string `json:"registry"`

	// Insecure allows connecting to the registry without TLS or with self-signed certs
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// AuthSecret is a reference to a Secret containing the credentials for the target registry.
	// The Secret should contain "username" and "password" or a ".dockerconfigjson".
	// +optional
	AuthSecret string `json:"authSecret,omitempty"`

	// Manager configuration for the mirroring manager pod.
	// +optional
	Manager PodConfig `json:"manager,omitempty"`

	// Worker configuration for the worker pods started by the manager.
	// +optional
	Worker PodConfig `json:"worker,omitempty"`

	// Concurrency controls how many worker pods may run in parallel.
	// Defaults to 20 if not set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Concurrency int `json:"concurrency,omitempty"`

	// BatchSize controls how many images are mirrored per worker pod.
	// A higher value reduces pod-start overhead at the cost of coarser failure granularity.
	// Defaults to 10 if not set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	BatchSize int `json:"batchSize,omitempty"`
}

// MirrorTargetStatus defines the observed state of MirrorTarget
type MirrorTargetStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MirrorTarget is the Schema for the mirrortargets API
type MirrorTarget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MirrorTargetSpec   `json:"spec,omitempty"`
	Status MirrorTargetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MirrorTargetList contains a list of MirrorTarget
type MirrorTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MirrorTarget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MirrorTarget{}, &MirrorTargetList{})
}
