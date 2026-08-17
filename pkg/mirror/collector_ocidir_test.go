package mirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// This file builds local OCI-layout ("ocidir://") fixtures directly through
// MirrorClient's Blob/Manifest calls, following the same pattern used in
// pkg/mirror/catalog/resolver_localdir_test.go and
// pkg/mirror/release/release_ocidir_test.go: regclient's ocidir scheme
// implements the Blob/Manifest read+write surface these collectors need, so
// real release payloads and operator catalogs can be built entirely on local
// disk with no HTTP wire-protocol fake required.

func ocidirBuildTarGz(t testing.TB, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0644})
		_, _ = tw.Write(content)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func ocidirPushBlob(t testing.TB, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, mt string, data []byte) descriptor.Descriptor {
	t.Helper()
	desc, err := client.BlobPut(ctx, r, descriptor.Descriptor{MediaType: mt, Size: int64(len(data))}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("BlobPut: %v", err)
	}
	return desc
}

func ocidirPushManifest(t testing.TB, ctx context.Context, client *mirrorclient.MirrorClient, r ref.Ref, orig any) manifest.Manifest {
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

// pushSingleManifestImage builds a one-manifest (non-list) image tagged "img"
// whose layers are exactly layerBlobs, in that order, and returns its
// ocidir:// reference string.
func pushSingleManifestImage(t testing.TB, layerBlobs [][]byte) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")

	dir := t.TempDir()
	r, err := ref.New("ocidir://" + dir + ":img")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	layerDescs := make([]descriptor.Descriptor, 0, len(layerBlobs))
	for _, lb := range layerBlobs {
		layerDescs = append(layerDescs, ocidirPushBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, lb))
	}
	cfgDesc := ocidirPushBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, []byte(`{}`))

	ocidirPushManifest(t, ctx, client, r, v1.Manifest{
		Versioned: v1.ManifestSchemaVersion,
		MediaType: mediatype.OCI1Manifest,
		Config:    cfgDesc,
		Layers:    layerDescs,
	})

	return "ocidir://" + dir + ":img"
}

// pushManifestListImage builds a manifest-list image tagged "img" in dir,
// with one child manifest per entry in perArchLayers (keyed by GOARCH), and
// returns its ocidir:// reference string. If skipArches is set, an index
// entry is still emitted for that arch but no child manifest is ever pushed
// for it, so resolving that platform fails.
func pushManifestListImage(t testing.TB, dir string, perArchLayers map[string][][]byte, skipArches map[string]bool) string {
	t.Helper()
	ctx := context.Background()
	client := mirrorclient.NewMirrorClient(nil, "")

	r, err := ref.New("ocidir://" + dir + ":img")
	if err != nil {
		t.Fatalf("ref.New: %v", err)
	}

	indexEntries := make([]descriptor.Descriptor, 0, len(perArchLayers))
	for arch, layerBlobs := range perArchLayers {
		if skipArches[arch] {
			indexEntries = append(indexEntries, descriptor.Descriptor{
				MediaType: mediatype.OCI1Manifest,
				Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				Size:      2,
				Platform:  &platform.Platform{OS: "linux", Architecture: arch},
			})
			continue
		}

		layerDescs := make([]descriptor.Descriptor, 0, len(layerBlobs))
		for _, lb := range layerBlobs {
			layerDescs = append(layerDescs, ocidirPushBlob(t, ctx, client, r, mediatype.OCI1LayerGzip, lb))
		}
		cfgDesc := ocidirPushBlob(t, ctx, client, r, mediatype.OCI1ImageConfig, []byte(`{"arch":"`+arch+`"}`))

		childM := ocidirPushManifest(t, ctx, client, r, v1.Manifest{
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

	ocidirPushManifest(t, ctx, client, r, v1.Index{
		Versioned: v1.IndexSchemaVersion,
		MediaType: mediatype.OCI1ManifestList,
		Manifests: indexEntries,
	})

	return "ocidir://" + dir + ":img"
}

const sampleImageReferencesJSON = `{
	"spec": {
		"tags": [
			{"name":"component-a","from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa"}}
		]
	}
}`

const sampleKubeVirtCMYAML = "data:\n  stream: '{\"architectures\":{\"x86_64\":{\"images\":{\"kubevirt\":{\"digest-ref\":\"quay.io/example/coreos@sha256:abc123\"}}}}}'\n"

// pushReleasePayload builds a single-arch release payload image carrying an
// image-references layer (for ExtractComponentImages) and, when
// withKubeVirt is true, a coreos-bootimages layer too (for
// ExtractKubeVirtImages).
func pushReleasePayload(t testing.TB, withKubeVirt bool) string {
	t.Helper()
	files := map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}
	if withKubeVirt {
		files["release-manifests/0000_50_installer_coreos-bootimages.yaml"] = []byte(sampleKubeVirtCMYAML)
	}
	layer := ocidirBuildTarGz(t, files)
	return pushSingleManifestImage(t, [][]byte{layer})
}

// buildFBCCatalogLayer returns a gzip-tar layer with a minimal FBC (package +
// single-entry channel + bundle with a related image) under configs/pkgName.
func buildFBCCatalogLayer(t testing.TB, pkgName string) []byte {
	t.Helper()
	yaml := "schema: olm.package\nname: " + pkgName + "\ndefaultChannel: stable\n---\n" +
		"schema: olm.channel\nname: stable\npackage: " + pkgName + "\nentries:\n  - name: " + pkgName + ".v1.0.0\n---\n" +
		"schema: olm.bundle\nname: " + pkgName + ".v1.0.0\npackage: " + pkgName + "\n" +
		"image: registry.example.com/" + pkgName + "-bundle@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000\n" +
		"relatedImages:\n  - name: extra\n    image: registry.example.com/" + pkgName + "-extra@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111\n" +
		"properties:\n  - type: olm.package\n    value:\n      packageName: " + pkgName + "\n      version: 1.0.0\n"
	return ocidirBuildTarGz(t, map[string][]byte{
		"configs/" + pkgName + "/catalog.yaml": []byte(yaml),
	})
}

// pushOperatorCatalog builds a single-manifest FBC catalog image for pkgName
// and returns its ocidir:// reference string.
func pushOperatorCatalog(t testing.TB, pkgName string) string {
	t.Helper()
	return pushSingleManifestImage(t, [][]byte{buildFBCCatalogLayer(t, pkgName)})
}
