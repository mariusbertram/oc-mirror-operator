package release

import (
	"context"
	"sort"
	"testing"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// The multi payload is a manifest list without a "linux/multi" entry; its
// metadata is read from the amd64 member.
func TestExtractComponentImages_MultiUsesAmd64Member(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	other := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(`{"spec":{"tags":[{"name":"wrong","from":{"name":"quay.io/x@sha256:zzz"}}]}}`)}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"amd64": {irLayer}, "arm64": {other}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractComponentImages(context.Background(), image, MultiArch)
	if err != nil {
		t.Fatalf("ExtractComponentImages(multi): %v", err)
	}
	if len(images) != 1 || images[0].Image != "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa" {
		t.Fatalf("images = %+v", images)
	}
}

// Without an amd64 member the first entry of the list is used.
func TestExtractComponentImages_MultiWithoutAmd64(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"arm64": {irLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractComponentImages(context.Background(), image, MultiArch)
	if err != nil || len(images) != 1 {
		t.Fatalf("images = %+v, err = %v", images, err)
	}
}

// A single-manifest payload is read as is, whatever the arch.
func TestExtractComponentImages_MultiOnSingleManifest(t *testing.T) {
	irLayer := buildTarGz(map[string][]byte{"release-manifests/image-references": []byte(sampleImageReferencesJSON)}).Bytes()
	image := pushSingleArchPayload(t, [][]byte{irLayer})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	if images, err := rr.ExtractComponentImages(context.Background(), image, MultiArch); err != nil || len(images) != 1 {
		t.Fatalf("images = %+v, err = %v", images, err)
	}
}

const multiKubeVirtCMYAML = "data:\n  stream: '{\"architectures\":{" +
	"\"x86_64\":{\"images\":{\"kubevirt\":{\"digest-ref\":\"quay.io/example/coreos@sha256:x86\"}}}," +
	"\"aarch64\":{\"images\":{\"kubevirt\":{\"digest-ref\":\"quay.io/example/coreos@sha256:arm\"}}}}}'\n"

// For the multi payload the KubeVirt disks of every architecture are needed.
func TestExtractKubeVirtImages_MultiTakesAllArchitectures(t *testing.T) {
	bootLayer := buildTarGz(map[string][]byte{
		"release-manifests/0000_50_installer_coreos-bootimages.yaml": []byte(multiKubeVirtCMYAML),
	}).Bytes()
	image := pushManifestListPayload(t, map[string][][]byte{"amd64": {bootLayer}, "arm64": {bootLayer}})

	rr := New(mirrorclient.NewMirrorClient(nil, ""))
	images, err := rr.ExtractKubeVirtImages(context.Background(), image, []string{MultiArch})
	if err != nil {
		t.Fatalf("ExtractKubeVirtImages(multi): %v", err)
	}
	sort.Strings(images)
	if len(images) != 2 || images[0] != "quay.io/example/coreos@sha256:arm" || images[1] != "quay.io/example/coreos@sha256:x86" {
		t.Fatalf("images = %v", images)
	}
}
