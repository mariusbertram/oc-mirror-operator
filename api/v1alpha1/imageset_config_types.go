package v1alpha1

// PlatformType defines the content type for platforms
// +kubebuilder:validation:Enum=ocp;okd
type PlatformType string

const (
	TypeOCP PlatformType = "ocp"
	TypeOKD PlatformType = "okd"
)

// Mirror defines the configuration for content types within the imageset.
type Mirror struct {
	// Platform defines the configuration for OpenShift and OKD platform types.
	// +optional
	Platform Platform `json:"platform,omitempty"`
	// Operators defines the configuration for Operator content types.
	// +optional
	Operators []Operator `json:"operators,omitempty"`
	// AdditionalImages defines the configuration for a list
	// of individual image content types.
	// +optional
	AdditionalImages []AdditionalImage `json:"additionalImages,omitempty"`
	// Helm define the configuration for Helm content types.
	// +optional
	Helm Helm `json:"helm,omitempty"`
	// BlockedImages define a list of images that will be blocked
	// from the mirroring process if they exist in other content
	// types in the configuration.
	// +optional
	BlockedImages []BlockedImage `json:"blockedImages,omitempty"`
	// Samples defines the configuration for Sample content types.
	// This is currently not implemented.
	// +optional
	Samples []SampleImage `json:"samples,omitempty"`
}

// Platform defines the configuration for OpenShift and OKD platform types.
type Platform struct {
	// Graph defines whether Cincinnati graph data will
	// downloaded and publish
	// +optional
	Graph bool `json:"graph,omitempty"`
	// Channels defines the configuration for individual
	// OCP and OKD channels
	// +optional
	Channels []ReleaseChannel `json:"channels,omitempty"`
	// Architectures defines one or more architectures
	// to mirror for the release image. This is defined at the
	// platform level to enable cross-channel upgrades.
	// +optional
	Architectures []string `json:"architectures,omitempty"`
	// This new field will allow the diskToMirror functionality
	// to copy from a release location on disk
	// +optional
	Release string `json:"release,omitempty"`
	// The kubeVirtContainer flag when set to true (default false)
	// will be used to extract the kubeVirtContainer image
	// from the release payload file 0000_50_installer_coreos-bootimages
	// +optional
	KubeVirtContainer bool `json:"kubeVirtContainer,omitempty"`
}

// ReleaseChannel defines the configuration for individual
// OCP and OKD channels
type ReleaseChannel struct {
	Name string `json:"name"`
	// Type of the platform in the context of this tool.
	// OCP is the default.
	// +optional
	// +kubebuilder:default=ocp
	Type PlatformType `json:"type,omitempty"`
	// MinVersion is minimum version in the
	// release channel to mirror
	// +optional
	MinVersion string `json:"minVersion,omitempty"`
	// MaxVersion is maximum version in the
	// release channel to mirror
	// +optional
	MaxVersion string `json:"maxVersion,omitempty"`
	// ShortestPath mode calculates the shortest path
	// between the min and mav version
	// +optional
	ShortestPath bool `json:"shortestPath,omitempty"`
	// Full mode set the MinVersion to the
	// first release in the channel and the MaxVersion
	// to the last release in the channel.
	// +optional
	Full bool `json:"full,omitempty"`
}

// Operator defines the configuration for operator catalog mirroring.
type Operator struct {
	// IncludeConfig defines specific operator packages to include.
	// +optional
	IncludeConfig `json:",inline"`
	// Catalog image to mirror.
	Catalog string `json:"catalog"`
	// TargetCatalog replaces TargetName and allows for specifying the exact URL of the target catalog.
	// +optional
	TargetCatalog string `json:"targetCatalog,omitempty"`
	// TargetTag is the tag the catalog image will be built with.
	// +optional
	TargetTag string `json:"targetTag,omitempty"`
	// Full defines whether all packages within the catalog will be mirrored.
	// +optional
	Full bool `json:"full,omitempty"`
	// SkipDependencies will not include dependencies if true.
	// +optional
	SkipDependencies bool `json:"skipDependencies,omitempty"`
}

// IncludeConfig defines a list of packages for
// operator version selection.
type IncludeConfig struct {
	// Packages to include.
	// +optional
	Packages []IncludePackage `json:"packages,omitempty"`
}

// IncludePackage contains a name (required) and channels and/or versions (optional).
type IncludePackage struct {
	// Name of package.
	Name string `json:"name"`
	// Channels to include.
	// +optional
	Channels []IncludeChannel `json:"channels,omitempty"`
	// +optional
	DefaultChannel string `json:"defaultChannel,omitempty"`

	// +optional
	IncludeBundle `json:",inline"`
}

// IncludeChannel contains a name (required) and versions (optional).
type IncludeChannel struct {
	// Name of channel.
	Name string `json:"name"`

	// +optional
	IncludeBundle `json:",inline"`
}

// IncludeBundle contains a name (required) and versions (optional).
type IncludeBundle struct {
	// MinVersion to include.
	// +optional
	MinVersion string `json:"minVersion,omitempty"`
	// MaxVersion to include.
	// +optional
	MaxVersion string `json:"maxVersion,omitempty"`
}

// AdditionalImage contains image pull information for additional images.
type AdditionalImage struct {
	// Name of the image.
	Name string `json:"name"`
	// TargetRepo replaces the repository path.
	// +optional
	TargetRepo string `json:"targetRepo,omitempty"`
	// TargetTag is the tag the image will be mirrored with.
	// +optional
	TargetTag string `json:"targetTag,omitempty"`
}

// BlockedImage contains image information used for excluding images.
type BlockedImage struct {
	Name string `json:"name"`
}

// SampleImage defines the configuration for Sample content types.
type SampleImage struct {
	Name string `json:"name"`
}

// Helm defines the configuration for Helm chart download and image mirroring.
type Helm struct {
	// Repositories are the Helm repositories containing the charts.
	// +optional
	Repositories []Repository `json:"repositories,omitempty"`
	// Local is the configuration for locally stored helm charts.
	// +optional
	Local []Chart `json:"local,omitempty"`
}

// Repository defines the configuration for a Helm repository.
type Repository struct {
	// URL is the url of the Helm repository.
	URL string `json:"url"`
	// Name is the name of the Helm repository.
	Name string `json:"name"`
	// Charts is a list of charts to pull from the repo.
	Charts []Chart `json:"charts"`
}

// Chart is the information an individual Helm chart.
type Chart struct {
	// Name is the chart name.
	Name string `json:"name"`
	// Version is the chart version.
	// +optional
	Version string `json:"version,omitempty"`
	// Path defines the path on disk where the chart is stored.
	// +optional
	Path string `json:"path,omitempty"`
	// ImagePaths are custom JSON paths for images location.
	// +optional
	ImagePaths []string `json:"imagePaths,omitempty"`
}
