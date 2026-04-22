package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageSetSpec defines the desired state of ImageSet
type ImageSetSpec struct {
	// Mirror defines the configuration for content types within the imageset.
	// This matches the ImageSetConfiguration of oc-mirror.
	// The ImageSet is associated with a MirrorTarget via the MirrorTarget's
	// spec.imageSets list — the ImageSet itself does not reference a target.
	Mirror Mirror `json:"mirror"`
}

// FailedImageDetail describes a single image that could not be mirrored.
type FailedImageDetail struct {
	// Source is the upstream image reference.
	Source string `json:"source"`
	// Destination is the mirroring target reference.
	Destination string `json:"destination"`
	// Error is the last error message from the mirroring attempt.
	// +optional
	Error string `json:"error,omitempty"`
	// Origin is a human-readable description of which spec entry produced
	// this image (e.g. "registry.../redhat-operator-index:v4.21 [web-terminal]"
	// or "stable-4.14 [amd64]").
	// +optional
	Origin string `json:"origin,omitempty"`
}

// ImageSetStatus defines the observed state of ImageSet
type ImageSetStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// TotalImages is the total number of images to be mirrored.
	// +optional
	TotalImages int `json:"totalImages,omitempty"`

	// MirroredImages is the number of images successfully mirrored.
	// +optional
	MirroredImages int `json:"mirroredImages,omitempty"`

	// PendingImages is the number of images waiting to be mirrored or in progress.
	// +optional
	PendingImages int `json:"pendingImages,omitempty"`

	// FailedImages is the number of images that failed mirroring (exhausted retries).
	// +optional
	FailedImages int `json:"failedImages,omitempty"`

	// FailedImageDetails lists the images that failed mirroring with their error
	// and the spec entry that referenced them. Capped at 20 entries to bound
	// status size; the FailedImages counter always reflects the full count.
	// +optional
	FailedImageDetails []FailedImageDetail `json:"failedImageDetails,omitempty"`

	// ObservedGeneration is the generation of the ImageSet that was last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastSuccessfulPollTime is the timestamp of the last successful upstream poll.
	// Used together with MirrorTarget.spec.pollInterval to schedule periodic re-collection.
	// +optional
	LastSuccessfulPollTime *metav1.Time `json:"lastSuccessfulPollTime,omitempty"`
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
