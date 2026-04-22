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

// ExposeType defines how the resource server is exposed.
// +kubebuilder:validation:Enum=Route;Ingress;GatewayAPI;Service
type ExposeType string

const (
	ExposeTypeRoute      ExposeType = "Route"
	ExposeTypeIngress    ExposeType = "Ingress"
	ExposeTypeGatewayAPI ExposeType = "GatewayAPI"
	ExposeTypeService    ExposeType = "Service"
)

// GatewayReference references a Gateway for HTTPRoute creation.
type GatewayReference struct {
	// Name of the Gateway resource.
	Name string `json:"name"`
	// Namespace of the Gateway resource (defaults to the MirrorTarget namespace).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ExposeConfig configures how the resource server HTTP endpoint is exposed.
// On OpenShift, if no explicit type is set, a Route is auto-created.
type ExposeConfig struct {
	// Type determines the exposure mechanism: Route (default on OpenShift),
	// Ingress, GatewayAPI, or Service (default on plain Kubernetes).
	// +optional
	Type ExposeType `json:"type,omitempty"`

	// Host is the external hostname for Route or Ingress.
	// Auto-generated if omitted (OpenShift Route only).
	// +optional
	Host string `json:"host,omitempty"`

	// IngressClassName selects the Ingress controller (only for type=Ingress).
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// GatewayRef references the Gateway resource (only for type=GatewayAPI).
	// +optional
	GatewayRef *GatewayReference `json:"gatewayRef,omitempty"`
}

// MirrorTargetSpec defines the desired state of MirrorTarget
type MirrorTargetSpec struct {
	// Registry is the URL of the target registry (e.g. registry.example.com/mirror)
	Registry string `json:"registry"`

	// ImageSets is the list of ImageSet names that should be mirrored to this target.
	// Each ImageSet must be in the same namespace as the MirrorTarget.
	// An ImageSet may only be referenced by a single MirrorTarget.
	// +optional
	ImageSets []string `json:"imageSets,omitempty"`

	// Insecure allows connecting to the registry without TLS or with self-signed certs
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// AuthSecret is a reference to a Secret containing the credentials for the target registry.
	// The Secret should contain "username" and "password" or a ".dockerconfigjson".
	// +optional
	AuthSecret string `json:"authSecret,omitempty"`

	// Expose configures how the resource server endpoint is exposed externally.
	// The resource server provides IDMS, ITMS, CatalogSource, ClusterCatalog,
	// and release signature resources via HTTP.
	// +optional
	Expose *ExposeConfig `json:"expose,omitempty"`

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

const (
	// CleanupPolicyAnnotation controls whether images are deleted from the target
	// registry when an ImageSet is removed from spec.imageSets.
	CleanupPolicyAnnotation = "mirror.openshift.io/cleanup-policy"
	// CleanupPolicyDelete triggers registry image deletion on ImageSet removal.
	CleanupPolicyDelete = "Delete"
)

// MirrorTargetStatus defines the observed state of MirrorTarget
type MirrorTargetStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// KnownImageSets is the last observed list of ImageSets from spec.imageSets.
	// Used internally to detect removals between reconcile cycles.
	// +optional
	KnownImageSets []string `json:"knownImageSets,omitempty"`

	// PendingCleanup lists ImageSet names whose images are currently being
	// deleted from the target registry by cleanup Jobs.
	// +optional
	PendingCleanup []string `json:"pendingCleanup,omitempty"`
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
