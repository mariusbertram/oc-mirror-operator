package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

const (
	MetadataConfigType = "application/vnd.mirror.openshift.io.config.v1+json"
	MetadataLayerType  = "application/vnd.mirror.openshift.io.metadata.v1+json"
)

// Metadata represents the state of mirrored images
type Metadata struct {
	MirroredImages map[string]string `json:"mirroredImages"` // destination -> digest
}

// registryClient is the subset of MirrorClient used by StateManager.
// Extracted as an interface so tests can provide a fake implementation.
type registryClient interface {
	ManifestGet(ctx context.Context, r ref.Ref) (manifest.Manifest, error)
	ManifestPut(ctx context.Context, r ref.Ref, m manifest.Manifest) error
	BlobGet(ctx context.Context, r ref.Ref, d descriptor.Descriptor) (blob.Reader, error)
	BlobPut(ctx context.Context, r ref.Ref, d descriptor.Descriptor, rdr io.Reader) (descriptor.Descriptor, error)
}

// StateManager handles reading and writing metadata to the target registry
type StateManager struct {
	client registryClient
}

func New(client *mirrorclient.MirrorClient) *StateManager {
	return &StateManager{client: client}
}

// NewWithClient creates a StateManager with a custom registryClient (for testing).
func NewWithClient(client registryClient) *StateManager {
	return &StateManager{client: client}
}

// ReadMetadata reads the metadata from the target registry at the given reference
func (s *StateManager) ReadMetadata(ctx context.Context, repository string, tag string) (*Metadata, string, error) {
	tagRef, err := ref.New(fmt.Sprintf("%s:%s", repository, tag))
	if err != nil {
		return nil, "", err
	}

	m, err := s.client.ManifestGet(ctx, tagRef)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get manifest: %w", err)
	}

	mi, ok := m.(manifest.Imager)
	if !ok {
		return nil, "", fmt.Errorf("manifest is not an image")
	}

	layers, err := mi.GetLayers()
	if err != nil || len(layers) == 0 {
		return nil, "", fmt.Errorf("no layers found in manifest")
	}

	// Find our metadata layer; fall back to the first layer
	layerDesc := layers[0]
	for _, l := range layers {
		if l.MediaType == MetadataLayerType {
			layerDesc = l
			break
		}
	}

	repoRef, err := ref.New(repository)
	if err != nil {
		return nil, "", err
	}

	blobReader, err := s.client.BlobGet(ctx, repoRef, layerDesc)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get blob: %w", err)
	}
	defer func() { _ = blobReader.Close() }()

	data, err := io.ReadAll(blobReader)
	if err != nil {
		return nil, "", err
	}

	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &meta, m.GetDescriptor().Digest.String(), nil
}

// WriteMetadata writes the metadata to the target registry
func (s *StateManager) WriteMetadata(ctx context.Context, repository string, tag string, meta *Metadata) (string, error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	repoRef, err := ref.New(repository)
	if err != nil {
		return "", err
	}

	// 1. Push metadata as a blob; digest must be pre-computed for BlobPut.
	dataDigest := godigest.FromBytes(data)
	layerDesc := descriptor.Descriptor{
		MediaType: MetadataLayerType,
		Digest:    dataDigest,
		Size:      int64(len(data)),
	}
	if _, err = s.client.BlobPut(ctx, repoRef, layerDesc, bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("failed to push metadata blob: %w", err)
	}

	// 2. Push an empty config blob.
	configData := []byte("{}")
	configDigest := godigest.FromBytes(configData)
	configDesc := descriptor.Descriptor{
		MediaType: MetadataConfigType,
		Digest:    configDigest,
		Size:      int64(len(configData)),
	}
	if _, err = s.client.BlobPut(ctx, repoRef, configDesc, bytes.NewReader(configData)); err != nil {
		return "", fmt.Errorf("failed to push config blob: %w", err)
	}

	// 3. Build and push the OCI manifest.
	ociM := v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    configDesc,
		Layers:    []descriptor.Descriptor{layerDesc},
	}
	m, err := manifest.New(manifest.WithOrig(ociM))
	if err != nil {
		return "", fmt.Errorf("failed to create manifest: %w", err)
	}

	tagRef, err := ref.New(fmt.Sprintf("%s:%s", repository, tag))
	if err != nil {
		return "", err
	}

	if err = s.client.ManifestPut(ctx, tagRef, m); err != nil {
		return "", fmt.Errorf("failed to push metadata manifest: %w", err)
	}

	return m.GetDescriptor().Digest.String(), nil
}
