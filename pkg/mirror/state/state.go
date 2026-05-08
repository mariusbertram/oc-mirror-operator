package state

const (
	MetadataConfigType = "application/vnd.mirror.openshift.io.config.v1+json"
	MetadataLayerType  = "application/vnd.mirror.openshift.io.metadata.v1+json"
)

// Metadata represents the state of mirrored images
type Metadata struct {
	MirroredImages map[string]string `json:"mirroredImages"` // destination -> digest
}
