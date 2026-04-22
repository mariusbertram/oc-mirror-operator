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

	// PollInterval defines how often the operator re-checks upstream sources
	// (release channels, operator catalogs) for new content.
	// New images are added as Pending; already-mirrored images are preserved.
	// Minimum: 1h. Default: 24h. Set to "0" to disable periodic polling.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || duration(self) >= duration('1h')",message="pollInterval must be 0s (disabled) or at least 1h"
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// CheckExistInterval defines how often the manager verifies that images
	// exist in the target registry. On manager startup the check always runs
	// immediately. For Mirrored images this detects drift (manual deletions).
	// For permanently-failed images (PermanentlyFailed=true) this triggers a
	// retry attempt in case the upstream error was transient.
	// Minimum: 1h. Default: 6h.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h')",message="checkExistInterval must be at least 1h"
	CheckExistInterval *metav1.Duration `json:"checkExistInterval,omitempty"`
}

const (
	// CleanupPolicyAnnotation controls whether images are deleted from the target
	// registry when an ImageSet is removed from spec.imageSets.
	CleanupPolicyAnnotation = "mirror.openshift.io/cleanup-policy"
	// CleanupPolicyDelete triggers registry image deletion on ImageSet removal.
	CleanupPolicyDelete = "Delete"

	// CatalogBuildSigAnnotation tracks the last input signature used to build
	// the operator catalog. Set on ImageSet by the controller after a catalog
	// build job completes successfully. When the computed signature changes,
	// the controller schedules a new catalog build.
	CatalogBuildSigAnnotation = "mirror.openshift.io/catalog-build-sig"

	// RecollectAnnotation forces the manager to re-resolve all upstream content
	// (releases, operator catalogs, additional images) on the next reconcile,
	// regardless of cached digests. The value is unused; presence is the trigger.
	// The annotation is removed by the manager once recollection completes so
	// that it is a one-shot trigger.
	RecollectAnnotation = "mirror.openshift.io/recollect"

	// CatalogDigestAnnotationPrefix is prepended to a stable signature hash of
	// a single operator-spec entry (catalog reference + sorted package list)
	// to form the annotation key that stores the last resolved upstream
	// manifest digest. Example:
	//   mirror.openshift.io/catalog-digest-<sha256-of-entry-sig>=sha256:abc...
	// Each spec.mirror.operators[] entry gets its own annotation, so the same
	// catalog can be referenced multiple times with different package filters
	// without collisions. The manager re-resolves a given entry only when the
	// upstream digest differs from the cached one OR the entry signature
	// changes (different packages, different catalog ref, …) which yields a
	// new annotation key altogether.
	CatalogDigestAnnotationPrefix = "mirror.openshift.io/catalog-digest-"

	// ReleaseDigestAnnotationPrefix is prepended to a stable signature hash of
	// a single release-channel-spec entry (channel name + sorted architectures
	// + min/max bounds + Full/ShortestPath flags + KubeVirt flag) to form the
	// annotation key that stores the last resolved channel-head digest.
	// Each spec.mirror.platform.channels[] entry gets its own annotation.
	ReleaseDigestAnnotationPrefix = "mirror.openshift.io/release-digest-"
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

	// TotalImages is the cumulative number of images across all ImageSets
	// referenced by this MirrorTarget. Aggregated from each ImageSet.Status.
	// +optional
	TotalImages int `json:"totalImages,omitempty"`

	// MirroredImages is the cumulative number of successfully mirrored images
	// across all ImageSets referenced by this MirrorTarget.
	// +optional
	MirroredImages int `json:"mirroredImages,omitempty"`

	// PendingImages is the cumulative number of images waiting to be mirrored
	// or currently in progress across all referenced ImageSets.
	// +optional
	PendingImages int `json:"pendingImages,omitempty"`

	// FailedImages is the cumulative number of images that failed mirroring
	// (exhausted retries) across all referenced ImageSets.
	// +optional
	FailedImages int `json:"failedImages,omitempty"`

	// ImageSetStatuses is a per-ImageSet breakdown of mirroring progress for
	// all ImageSets referenced by spec.imageSets. Entries appear in
	// alphabetical order. Missing ImageSets are reported with Found=false.
	// +optional
	ImageSetStatuses []ImageSetStatusSummary `json:"imageSetStatuses,omitempty"`
}

// ImageSetStatusSummary is a per-ImageSet snapshot of mirroring progress
// surfaced on the MirrorTarget so users can see overall rollout state at a
// glance without listing the individual ImageSet objects.
type ImageSetStatusSummary struct {
	// Name is the ImageSet name (matches an entry in spec.imageSets).
	Name string `json:"name"`

	// Found is true if the referenced ImageSet exists in this namespace.
	// When false the counters are zero and the entry indicates a missing
	// reference (typo in spec.imageSets or ImageSet not yet created).
	Found bool `json:"found"`

	// Total is the number of images this ImageSet is responsible for.
	// +optional
	Total int `json:"total,omitempty"`

	// Mirrored is the number of images successfully mirrored.
	// +optional
	Mirrored int `json:"mirrored,omitempty"`

	// Pending is the number of images waiting to be mirrored or in progress.
	// +optional
	Pending int `json:"pending,omitempty"`

	// Failed is the number of images that failed mirroring (exhausted retries).
	// +optional
	Failed int `json:"failed,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalImages`
// +kubebuilder:printcolumn:name="Mirrored",type=integer,JSONPath=`.status.mirroredImages`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingImages`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedImages`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
