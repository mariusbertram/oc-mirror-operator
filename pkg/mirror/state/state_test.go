package state

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	godigest "github.com/opencontainers/go-digest"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	ociV1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

// fakeRegistryClient implements registryClient for testing.
type fakeRegistryClient struct {
	manifests map[string]manifest.Manifest // ref string → manifest
	blobs     map[string][]byte            // digest string → data

	manifestPutErr error
	blobPutErr     error
	blobGetErr     error
	manifestGetErr error
}

func newFakeRegistryClient() *fakeRegistryClient {
	return &fakeRegistryClient{
		manifests: make(map[string]manifest.Manifest),
		blobs:     make(map[string][]byte),
	}
}

func (f *fakeRegistryClient) ManifestGet(_ context.Context, r ref.Ref) (manifest.Manifest, error) {
	if f.manifestGetErr != nil {
		return nil, f.manifestGetErr
	}
	key := r.CommonName()
	m, ok := f.manifests[key]
	if !ok {
		return nil, fmt.Errorf("manifest not found: %s", key)
	}
	return m, nil
}

func (f *fakeRegistryClient) ManifestPut(_ context.Context, _ ref.Ref, _ manifest.Manifest) error {
	return f.manifestPutErr
}

func (f *fakeRegistryClient) BlobGet(_ context.Context, _ ref.Ref, d descriptor.Descriptor) (blob.Reader, error) {
	if f.blobGetErr != nil {
		return nil, f.blobGetErr
	}
	data, ok := f.blobs[d.Digest.String()]
	if !ok {
		return nil, fmt.Errorf("blob not found: %s", d.Digest.String())
	}
	return blob.NewReader(
		blob.WithReader(io.NopCloser(io.NewSectionReader(
			readerAtFromBytes(data), 0, int64(len(data)),
		))),
		blob.WithDesc(d),
	), nil
}

func (f *fakeRegistryClient) BlobPut(_ context.Context, _ ref.Ref, _ descriptor.Descriptor, _ io.Reader) (descriptor.Descriptor, error) {
	if f.blobPutErr != nil {
		return descriptor.Descriptor{}, f.blobPutErr
	}
	return descriptor.Descriptor{}, nil
}

// readerAtFromBytes wraps a byte slice as an io.ReaderAt.
type bytesReaderAt struct{ data []byte }

func (b *bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func readerAtFromBytes(data []byte) io.ReaderAt {
	return &bytesReaderAt{data: data}
}

// buildFakeManifest creates a minimal OCI manifest with a metadata layer.
func buildFakeManifest(metadataDigest godigest.Digest, metadataSize int64) (manifest.Manifest, error) {
	configDigest := godigest.FromBytes([]byte("{}"))
	ociM := ociV1.Manifest{
		Versioned: ociV1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config: descriptor.Descriptor{
			MediaType: MetadataConfigType,
			Digest:    configDigest,
			Size:      2,
		},
		Layers: []descriptor.Descriptor{
			{
				MediaType: MetadataLayerType,
				Digest:    metadataDigest,
				Size:      metadataSize,
			},
		},
	}
	return manifest.New(manifest.WithOrig(ociM))
}

var _ = Describe("State", func() {
	Context("New", func() {
		It("returns a non-nil StateManager", func() {
			mc := mirrorclient.NewMirrorClient(nil, "")
			sm := New(mc)
			Expect(sm).NotTo(BeNil())
			Expect(sm.client).NotTo(BeNil())
		})
	})

	Context("NewWithClient", func() {
		It("returns a non-nil StateManager with fake client", func() {
			fc := newFakeRegistryClient()
			sm := NewWithClient(fc)
			Expect(sm).NotTo(BeNil())
		})
	})

	Context("Metadata JSON round-trip", func() {
		It("serializes and deserializes correctly", func() {
			meta := &Metadata{
				MirroredImages: map[string]string{
					"registry.io/img:v1": "sha256:aaa",
					"registry.io/img:v2": "sha256:bbb",
				},
			}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(HaveLen(2))
			Expect(decoded.MirroredImages["registry.io/img:v1"]).To(Equal("sha256:aaa"))
		})

		It("handles empty MirroredImages", func() {
			meta := &Metadata{MirroredImages: map[string]string{}}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(BeEmpty())
		})

		It("handles nil MirroredImages", func() {
			meta := &Metadata{}
			data, err := json.Marshal(meta)
			Expect(err).NotTo(HaveOccurred())

			var decoded Metadata
			Expect(json.Unmarshal(data, &decoded)).To(Succeed())
			Expect(decoded.MirroredImages).To(BeNil())
		})
	})

	Context("ReadMetadata", func() {
		It("returns error for invalid repository reference", func() {
			sm := NewWithClient(newFakeRegistryClient())
			_, _, err := sm.ReadMetadata(context.TODO(), ":::invalid", "latest")
			Expect(err).To(HaveOccurred())
		})

		It("returns error when ManifestGet fails", func() {
			fc := newFakeRegistryClient()
			fc.manifestGetErr = fmt.Errorf("network error")
			sm := NewWithClient(fc)
			_, _, err := sm.ReadMetadata(context.TODO(), "localhost/meta", "latest")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get manifest"))
		})

		It("returns error when BlobGet fails", func() {
			fc := newFakeRegistryClient()
			meta := &Metadata{MirroredImages: map[string]string{"a": "b"}}
			data, _ := json.Marshal(meta)
			digest := godigest.FromBytes(data)
			m, err := buildFakeManifest(digest, int64(len(data)))
			Expect(err).NotTo(HaveOccurred())
			fc.manifests["localhost/meta:latest"] = m
			fc.blobGetErr = fmt.Errorf("blob not available")

			sm := NewWithClient(fc)
			_, _, err = sm.ReadMetadata(context.TODO(), "localhost/meta", "latest")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get blob"))
		})

		It("returns error when blob contains invalid JSON", func() {
			fc := newFakeRegistryClient()
			badData := []byte("not-json")
			digest := godigest.FromBytes(badData)
			m, err := buildFakeManifest(digest, int64(len(badData)))
			Expect(err).NotTo(HaveOccurred())
			fc.manifests["localhost/meta:latest"] = m
			fc.blobs[digest.String()] = badData

			sm := NewWithClient(fc)
			_, _, err = sm.ReadMetadata(context.TODO(), "localhost/meta", "latest")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to unmarshal"))
		})

		It("successfully reads metadata", func() {
			fc := newFakeRegistryClient()
			meta := &Metadata{MirroredImages: map[string]string{
				"reg.io/img:v1": "sha256:abc",
			}}
			data, _ := json.Marshal(meta)
			digest := godigest.FromBytes(data)
			m, err := buildFakeManifest(digest, int64(len(data)))
			Expect(err).NotTo(HaveOccurred())
			fc.manifests["localhost/meta:latest"] = m
			fc.blobs[digest.String()] = data

			sm := NewWithClient(fc)
			result, digestStr, err := sm.ReadMetadata(context.TODO(), "localhost/meta", "latest")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.MirroredImages).To(HaveKeyWithValue("reg.io/img:v1", "sha256:abc"))
			Expect(digestStr).NotTo(BeEmpty())
		})
	})

	Context("WriteMetadata", func() {
		It("returns error for invalid repository reference", func() {
			sm := NewWithClient(newFakeRegistryClient())
			meta := &Metadata{MirroredImages: map[string]string{"a": "b"}}
			_, err := sm.WriteMetadata(context.TODO(), ":::invalid", "latest", meta)
			Expect(err).To(HaveOccurred())
		})

		It("returns error when BlobPut for metadata fails", func() {
			fc := newFakeRegistryClient()
			fc.blobPutErr = fmt.Errorf("push failed")
			sm := NewWithClient(fc)
			meta := &Metadata{MirroredImages: map[string]string{"a": "b"}}
			_, err := sm.WriteMetadata(context.TODO(), "localhost/meta", "latest", meta)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to push metadata blob"))
		})

		It("returns error when ManifestPut fails", func() {
			fc := newFakeRegistryClient()
			fc.manifestPutErr = fmt.Errorf("manifest push failed")
			sm := NewWithClient(fc)
			meta := &Metadata{MirroredImages: map[string]string{"a": "b"}}
			_, err := sm.WriteMetadata(context.TODO(), "localhost/meta", "latest", meta)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to push metadata manifest"))
		})

		It("successfully writes metadata", func() {
			fc := newFakeRegistryClient()
			sm := NewWithClient(fc)
			meta := &Metadata{MirroredImages: map[string]string{
				"reg.io/img:v1": "sha256:abc",
			}}
			digestStr, err := sm.WriteMetadata(context.TODO(), "localhost/meta", "latest", meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(digestStr).NotTo(BeEmpty())
		})
	})

	Context("Constants", func() {
		It("has expected media types", func() {
			Expect(MetadataConfigType).To(Equal("application/vnd.mirror.openshift.io.config.v1+json"))
			Expect(MetadataLayerType).To(Equal("application/vnd.mirror.openshift.io.metadata.v1+json"))
		})
	})
})
