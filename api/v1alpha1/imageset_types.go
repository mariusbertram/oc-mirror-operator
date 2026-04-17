package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageSetSpec defines the desired state of ImageSet
type ImageSetSpec struct {
	// TargetRef is a reference to a MirrorTarget CR.
	TargetRef string `json:"targetRef"`

	// Mirror defines the configuration for content types within the imageset.
	// This matches the ImageSetConfiguration of oc-mirror.
	Mirror Mirror `json:"mirror"`
}

// ImageSetStatus defines the observed state of ImageSet
type ImageSetStatus struct {
	// TargetImages is the list of images that need to be mirrored.
	// This is generated from the ImageSet configuration.
	// +optional
	TargetImages []TargetImageStatus `json:"targetImages,omitempty"`

	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// TotalImages is the total number of images to be mirrored.
	// +optional
	TotalImages int `json:"totalImages,omitempty"`

	// MirroredImages is the number of images successfully mirrored.
	// +optional
	MirroredImages int `json:"mirroredImages,omitempty"`

	// StateDigest is the digest of the OCI metadata blob in the target registry.
	// +optional
	StateDigest string `json:"stateDigest,omitempty"`

	// ObservedGeneration is the generation of the ImageSet that was last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// TargetImageStatus defines the observed state of a single image mirroring.
type TargetImageStatus struct {
	// Source is the source image reference.
	Source string `json:"source"`
	// Destination is the destination image reference in the target registry.
	Destination string `json:"destination"`
	// State is the current state of the mirroring for this image.
	// Pending, Mirrored, Failed, Skipped
	State string `json:"state"`
	// LastError is the last error message if the mirroring failed.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ImageSet is the Schema for the imagesets API
type ImageSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageSetSpec   `json:"spec,omitempty"`
	Status ImageSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageSetList contains a list of ImageSet
type ImageSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageSet{}, &ImageSetList{})
}
