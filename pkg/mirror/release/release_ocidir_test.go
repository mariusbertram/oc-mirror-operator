package release

import (
	"bytes"
	"context"
	"testing"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
)

// This file builds local OCI-layout ("ocidir://") release-payload fixtures
// directly through MirrorClient's Blob/Manifest calls to exercise
// ExtractComponentImages/ExtractKubeVirtImages against a real manifest +
// layer pipeline, following the same approach as
// pkg/mirror/catalog/resolver_localdir_test.go: regclient's ocidir scheme
// implements the Blob/Manifest Get/Put surface these functions need, so no
// HTTP wire-protocol fake is required.

func pushBlob(t *testing.T, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, mt string, data []byte) descriptor.Descriptor {
	t.Helper()
	desc, err := client.BlobPut(ctx, r, descriptor.Descriptor{MediaType: mt, Size: int64(len(data))}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("BlobPut: %v", err)
	}
	return desc
}

func pushManifest(t *testing.T, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, orig any) manifest.Manifest {
	t.Helper()
	m, err := manifest.New(manifest.WithOrig(orig))
	if err != nil {
		t.Fatalf("manifest.New: %v", err)
	}
	if err := client.ManifestPut(ctx, r, m); err != nil {
		t.Fatalf("ManifestPut: %v", err)
	}
	return m
}

// pushSingleArchPayload builds a one-manifest (non-list) release payload
// image tagged "release" whose layers are exactly layerBlobs, in that
// order, and returns its ocidir:// reference string.
func pushSingleArchPayload(t *testing.T, layerBlobs [][]byte) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")

	dir := t.TempDir()
	r, err := ref.New("ocidir://" + dir + ":release")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	layerDescs := make([]descriptor.Descriptor, 0, len(layerBlobs))
	for _, lb := range layerBlobs {
		layerDescs = append(layerDescs, pushBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, lb))
	}

	cfgDesc := pushBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, []byte(`{}`))

	pushManifest(t, ctx, client, r, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    layerDescs,
	})

	return "ocidir://" + dir + ":release"
}

// pushManifestListPayload builds a manifest-list release payload tagged
// "release" with one child manifest per entry in perArchLayers (keyed by
// GOARCH), and returns its ocidir:// reference string.
func pushManifestListPayload(t *testing.T, perArchLayers map[string][][]byte) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")

	dir := t.TempDir()
	r, err := ref.New("ocidir://" + dir + ":release")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	indexEntries := make([]descriptor.Descriptor, 0, len(perArchLayers))
	for arch, layerBlobs := range perArchLayers {
		layerDescs := make([]descriptor.Descriptor, 0, len(layerBlobs))
		for _, lb := range layerBlobs {
			layerDescs = append(layerDescs, pushBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, lb))
		}
		cfgDesc := pushBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, []byte(`{"arch":"`+arch+`"}`))

		childM := pushManifest(t, ctx, client, r, v1.Manifest{
			Versioned: v1.ManifestSchemaVersion,
			MediaType: mediatype.OCI1Manifest,
			Config:    cfgDesc,
			Layers:    layerDescs,
		})
		childDesc := childM.GetDescriptor()

		childRef := r
		childRef.Digest = childDesc.Digest.String()
		childRef.Tag = ""
		if err := client.ManifestPut(ctx, childRef, childM); err != nil {
			t.Fatalf("ManifestPut child: %v", err)
		}

		indexEntries = append(indexEntries, descriptor.Descriptor{
			MediaType: mediatype.OCI1Manifest,
			Digest:    childDesc.Digest,
			Size:      childDesc.Size,
			Platform:  &platform.Platform{OS: "linux", Architecture: arch},
		})
	}

	pushManifest(t, ctx, client, r, v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: mediatype.OCI1ManifestList,
		Manifests: indexEntries,
	})

	return "ocidir://" + dir + ":release"
}

const sampleImageReferencesJSON = `{
	"spec": {
		"tags": [
			{"name":"component-a","from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa"}}
		]
	}
}`

func TestExtractComponentImages_SingleArch_Success(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	unrelated := buildTarGz(map[string][]byte{"some/other/file": []byte("x")}).Bytes()
	image := pushSingleArchPayload(t, [][]byte{unrelated, irLayer})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractComponentImages(context.Background(), image, "amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 || images[0].Name != "component-a" {
		t.Errorf("unexpected images: %+v", images)
	}
}

func TestExtractComponentImages_ManifestList_ResolvesPlatform(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	otherLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(`{"spec":{"tags":[{"name":"wrong-arch","from":{"name":"quay.io/x@sha256:zzz"}}]}}`)}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{
		"amd64": {irLayer},
		"arm64": {otherLayer},
	})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractComponentImages(context.Background(), image, "amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 || images[0].Name != "component-a" {
		t.Errorf("expected amd64 payload's images, got: %+v", images)
	}
}

func TestExtractComponentImages_ManifestList_PlatformNotFound(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"arm64": {irLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractComponentImages(context.Background(), image, "amd64")
	if err == nil {
		t.Fatal("expected an error when the requested platform is absent from the list")
	}
}

func TestExtractComponentImages_NoLayers(t *testing.T) {
	image := pushSingleArchPayload(t, nil)

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractComponentImages(context.Background(), image, "amd64")
	if err == nil {
		t.Fatal("expected an error for a payload with no layers")
	}
}

func TestExtractComponentImages_NotFoundInAnyLayer(t *testing.T) {
	unrelated := buildTarGz(map[string][]byte{"some/other/file": []byte("x")}).Bytes()
	image := pushSingleArchPayload(t, [][]byte{unrelated})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractComponentImages(context.Background(), image, "amd64")
	if err == nil {
		t.Fatal("expected an error when no layer contains image-references")
	}
}

func TestExtractComponentImages_ManifestGetFails(t *testing.T) {
	dir := t.TempDir()
	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractComponentImages(context.Background(), "ocidir://"+dir+":missing-tag", "amd64")
	if err == nil {
		t.Fatal("expected an error for a non-existent manifest")
	}
}

const sampleKubeVirtCMYAML = "data:\n  stream: '{\"architectures\":{\"x86_64\":{\"images\":{\"kubevirt\":{\"digest-ref\":\"quay.io/example/coreos@sha256:abc123\"}}}}}'\n"

func TestExtractKubeVirtImages_SingleArch_Success(t *testing.T) {
	bootLayer := buildTarGz(map[string][]byte{
		"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(sampleKubeVirtCMYAML),
	}).Bytes()
	image := pushSingleArchPayload(t, [][]byte{bootLayer})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractKubeVirtImages(context.Background(), image, []string{"amd64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 || images[0] != "quay.io/example/coreos@sha256:abc123" {
		t.Errorf("unexpected images: %+v", images)
	}
}

func TestExtractKubeVirtImages_ManifestList_ResolvesFirstArch(t *testing.T) {
	bootLayer := buildTarGz(map[string][]byte{
		"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(sampleKubeVirtCMYAML),
	}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"amd64": {bootLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractKubeVirtImages(context.Background(), image, []string{"amd64", "arm64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 || images[0] != "quay.io/example/coreos@sha256:abc123" {
		t.Errorf("unexpected images: %+v", images)
	}
}

func TestExtractKubeVirtImages_DefaultsToAmd64WhenArchesEmpty(t *testing.T) {
	bootLayer := buildTarGz(map[string][]byte{
		"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(sampleKubeVirtCMYAML),
	}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"amd64": {bootLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractKubeVirtImages(context.Background(), image, nil)
	if err != nil {
		t.Fatalf("unexpected error resolving the default amd64 platform: %v", err)
	}
}

func TestExtractKubeVirtImages_NotFoundInAnyLayer(t *testing.T) {
	unrelated := buildTarGz(map[string][]byte{"some/other/file": []byte("x")}).Bytes()
	image := pushSingleArchPayload(t, [][]byte{unrelated})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractKubeVirtImages(context.Background(), image, []string{"amd64"})
	if err == nil {
		t.Fatal("expected an error when no layer contains coreos-bootimages")
	}
}

func TestExtractKubeVirtImages_ManifestGetFails(t *testing.T) {
	dir := t.TempDir()
	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractKubeVirtImages(context.Background(), "ocidir://"+dir+":missing-tag", []string{"amd64"})
	if err == nil {
		t.Fatal("expected an error for a non-existent manifest")
	}
}

func TestExtractKubeVirtImages_ManifestList_PlatformNotFound(t *testing.T) {
	bootLayer := buildTarGz(map[string][]byte{
		"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(sampleKubeVirtCMYAML),
	}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"arm64": {bootLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	_, err := rr.ExtractKubeVirtImages(context.Background(), image, []string{"amd64"})
	if err == nil {
		t.Fatal("expected an error when the requested platform is absent from the list")
	}
}
