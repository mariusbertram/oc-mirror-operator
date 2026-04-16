package state

import (
	"context"
	"encoding/json"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// Metadata represents the state of mirrored images
type Metadata struct {
	MirroredImages map[string]string `json:"mirroredImages"` // destination -> digest
}

// StateManager handles reading and writing metadata to the target registry
type StateManager struct {
	client *mirrorclient.MirrorClient
}

func New(client *mirrorclient.MirrorClient) *StateManager {
	return &StateManager{client: client}
}

// ReadMetadata reads the metadata from the target registry at the given reference
func (s *StateManager) ReadMetadata(ctx context.Context, repository string, tag string) (*Metadata, string, error) {
	// In a real implementation, we would use manifest and blob get to retrieve the json.
	// For now, this is a placeholder that shows the intent.
	_ = repository
	_ = tag

	return &Metadata{MirroredImages: make(map[string]string)}, "", nil
}

// WriteMetadata writes the metadata to the target registry
func (s *StateManager) WriteMetadata(ctx context.Context, repository string, tag string, meta *Metadata) (string, error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	// Use regclient to push the data as a blob and create a manifest pointing to it.
	// For this prototype, we'll keep it simple.
	_ = data
	_ = repository
	_ = tag

	return "sha256:dummy", nil
}
