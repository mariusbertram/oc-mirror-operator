/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MirrorExportSpec defines the desired state of MirrorExport.
//
// Unlike MirrorTarget/ImageSet, a MirrorExport is not continuously
// reconciled into a live mirror: it describes content to resolve for a
// disconnected (air-gapped) destination that the reconciling cluster cannot
// necessarily reach. Reconciling it produces downloadable artifacts (a
// resolved image manifest, IDMS/ITMS/CatalogSource/ClusterCatalog resources,
// and a build spec for the operator-catalog/graph-data content that must be
// built rather than copied) — see docs/mirror2disk.md.
type MirrorExportSpec struct {
	// Mirror defines the content to resolve — identical schema to
	// ImageSet.spec.mirror (release channels, operator catalogs, additional
	// images, Helm charts).
	Mirror Mirror `json:"mirror"`

	// Source overrides where resolution pulls from. Omitted means upstream
	// (registry.redhat.io, quay.io, the Cincinnati graph API, and catalog
	// refs exactly as specified in Mirror.Operators[].Catalog). Set this to
	// resolve against an already-populated local/connected mirror registry
	// instead of pulling the same content from the public internet again.
	// +optional
	Source *MirrorExportSource `json:"source,omitempty"`

	// Destination is the registry the resolved manifest's destinations, and
	// the generated IDMS/ITMS/CatalogSource/ClusterCatalog resources, are
	// computed against. The reconciling cluster does not need to be able to
	// reach it — it is only used to compute destination image references.
	Destination MirrorExportDestination `json:"destination"`
}

// MirrorExportSource overrides the registry that content resolution pulls
// from. Mirrors the shape of the credential/TLS fields already used for the
// target registry on MirrorTargetSpec.
type MirrorExportSource struct {
	// Registry is the URL of a source registry to resolve against instead of
	// upstream (e.g. an already-populated local mirror registry).
	// +optional
	Registry string `json:"registry,omitempty"`

	// Insecure allows connecting to the source registry without TLS or with
	// self-signed certs.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// AuthSecret is a reference to a Secret containing the credentials for
	// the source registry. The Secret should contain "username" and
	// "password" or a ".dockerconfigjson".
	// +optional
	AuthSecret string `json:"authSecret,omitempty"`

	// CABundle references a ConfigMap in the same namespace that contains a
	// PEM-encoded CA certificate bundle for the source registry.
	// +optional
	CABundle *CABundleRef `json:"caBundle,omitempty"`
}

// MirrorExportDestination is the registry the resolved artifacts' destination
// image references are computed against.
type MirrorExportDestination struct {
	// Registry is the URL of the destination registry (e.g.
	// registry.example.com/mirror). This registry does not need to be
	// reachable from the cluster reconciling the MirrorExport.
	Registry string `json:"registry"`

	// Insecure marks the destination registry as not requiring TLS/using a
	// self-signed cert, reflected into the generated IDMS/ITMS resources.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// MirrorExportStatus defines the observed state of MirrorExport.
type MirrorExportStatus struct {
	// Conditions represent the latest available observations of the export's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation of the MirrorExport that was last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// TotalImages is the number of plain images in the last successfully
	// rendered resolved image manifest.
	// +optional
	TotalImages int `json:"totalImages,omitempty"`

	// ArtifactsConfigMap is the name of the ConfigMap (in the same namespace)
	// containing the rendered artifacts: manifest.json, idms.yaml, itms.yaml,
	// catalogsource-<slug>.yaml / clustercatalog-<slug>.yaml per operator
	// entry, and buildspec.json.
	// +optional
	ArtifactsConfigMap string `json:"artifactsConfigMap,omitempty"`

	// LastRenderedSignature is a hash of the spec content that produced the
	// current artifacts, used to detect when a re-render is needed.
	// +optional
	LastRenderedSignature string `json:"lastRenderedSignature,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalImages`
// +kubebuilder:printcolumn:name="Artifacts",type=string,JSONPath=`.status.artifactsConfigMap`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MirrorExport is the Schema for the mirrorexports API
type MirrorExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MirrorExportSpec   `json:"spec,omitempty"`
	Status MirrorExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MirrorExportList contains a list of MirrorExport
type MirrorExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MirrorExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MirrorExport{}, &MirrorExportList{})
}
