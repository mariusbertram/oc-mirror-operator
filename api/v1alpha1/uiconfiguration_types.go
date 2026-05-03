package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UIExposureType defines how the UI dashboard is exposed.
// +kubebuilder:validation:Enum=service;ingress;route;consolePlugin
type UIExposureType string

const (
	UIExposureTypeService       UIExposureType = "service"
	UIExposureTypeIngress       UIExposureType = "ingress"
	UIExposureTypeRoute         UIExposureType = "route"
	UIExposureTypeConsolePlugin UIExposureType = "consolePlugin"
)

// UITLSConfig defines TLS configuration for the UI dashboard.
type UITLSConfig struct {
	// Enabled controls whether TLS is enabled for the UI dashboard.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// CertSecretRef is a reference to a Kubernetes Secret containing the
	// TLS certificate and key. The Secret must contain "tls.crt" and "tls.key" fields.
	// Required if Enabled is true and IssuerRef is not set.
	// +optional
	CertSecretRef *corev1.LocalObjectReference `json:"certSecretRef,omitempty"`

	// IssuerRef is an optional reference to a cert-manager Issuer or ClusterIssuer
	// for automatic certificate generation and rotation.
	// Not currently implemented; documented for future extensibility.
	// +optional
	IssuerRef *UIIssuerRef `json:"issuerRef,omitempty"`
}

// UIIssuerRef references a cert-manager Issuer or ClusterIssuer.
// Not currently implemented; documented for future extensibility.
type UIIssuerRef struct {
	// Name of the Issuer or ClusterIssuer.
	Name string `json:"name"`

	// Kind is either "Issuer" or "ClusterIssuer". Defaults to "Issuer".
	// +optional
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	Kind string `json:"kind,omitempty"`
}

// UIConfigurationSpec defines the desired state of UIConfiguration
type UIConfigurationSpec struct {
	// ExposureType determines how the UI dashboard is exposed.
	// Valid values are: "service", "ingress", "route", "consolePlugin".
	// "service" exposes the dashboard via a Kubernetes Service (default).
	// "ingress" exposes via an Ingress resource (requires TLS).
	// "route" exposes via an OpenShift Route (OpenShift only).
	// "consolePlugin" integrates the dashboard as an OpenShift Console Plugin (OpenShift only).
	// +kubebuilder:validation:Required
	// +kubebuilder:default=service
	ExposureType UIExposureType `json:"exposureType"`

	// TLS configures TLS/HTTPS for the UI dashboard.
	// +optional
	TLS *UITLSConfig `json:"tls,omitempty"`

	// Resources defines the compute resources (CPU/memory requests and limits)
	// for the UI dashboard pod.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Replicas is the desired number of UI dashboard pod replicas.
	// Defaults to 1.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Hostname is the external hostname for the UI dashboard when using
	// Ingress or Route exposure types. Auto-generated if omitted.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// IngressClassName selects the Ingress controller for type=ingress.
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// RouteName is the name of the Route resource to create (OpenShift only, for type=route).
	// Auto-generated if omitted.
	// +optional
	RouteName string `json:"routeName,omitempty"`
}

// UIConfigurationPhase represents the current phase of UIConfiguration reconciliation.
// +kubebuilder:validation:Enum=pending;active;failed
type UIConfigurationPhase string

const (
	UIConfigurationPhasePending UIConfigurationPhase = "pending"
	UIConfigurationPhaseActive  UIConfigurationPhase = "active"
	UIConfigurationPhaseFailed  UIConfigurationPhase = "failed"
)

// UIConfigurationStatus defines the observed state of UIConfiguration
type UIConfigurationStatus struct {
	// ObservedGeneration reflects the generation of the spec that has been observed
	// by the controller. This is used to detect when the spec has been updated.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the UIConfiguration's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ExposedURL is the actual URL where the UI dashboard is accessible.
	// Set once the dashboard is successfully exposed and ready.
	// +optional
	ExposedURL string `json:"exposedURL,omitempty"`

	// Phase represents the current reconciliation phase of the UIConfiguration.
	// Values are "pending" (initial state), "active" (successfully deployed),
	// or "failed" (reconciliation failed).
	// +optional
	Phase UIConfigurationPhase `json:"phase,omitempty"`

	// AvailableReplicas is the number of UI dashboard pods currently ready.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// DesiredReplicas is the desired number of UI dashboard pods.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=uic
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Exposure Type",type=string,JSONPath=`.spec.exposureType`
// +kubebuilder:printcolumn:name="Exposed URL",type=string,JSONPath=`.status.exposedURL`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// UIConfiguration is the Schema for the uiconfigurations API.
// UIConfiguration is namespace-scoped and controls how the oc-mirror UI dashboard is exposed
// and configured (via Service, Ingress, Route, or OpenShift Console Plugin).
type UIConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UIConfigurationSpec   `json:"spec,omitempty"`
	Status UIConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UIConfigurationList contains a list of UIConfiguration
type UIConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UIConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UIConfiguration{}, &UIConfigurationList{})
}
