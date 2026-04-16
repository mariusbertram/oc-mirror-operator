package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
