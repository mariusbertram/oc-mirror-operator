package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const ociIndexMediaType = "application/vnd.oci.image.index.v1+json"

// referrersFallbackTag returns the tag regclient uses when the referrers API
// is unavailable: sha256-<hex of the subject digest>.
func referrersFallbackTag(manifestDigest string) string {
	return strings.Replace(manifestDigest, ":", "-", 1)
}

// TestCopyImage_SucceedsWhenReferrersFallbackTagIsNotAnIndex reproduces #278:
// registry.gitlab.com has no referrers API and answers the fallback tag
// sha256-<hex> with a plain (non-index) manifest. regclient's
// referrerListByTag errors with "manifest is not an OCI index", which must not
// fail the image copy.
func TestCopyImage_SucceedsWhenReferrersFallbackTagIsNotAnIndex(t *testing.T) {
	srcReg, srcHost := newFakeRegistryServer(t)
	manifestDigest, _ := seedTestImage(t, srcReg, "gitlab-org/build/cng/sidekiq-ee", "v18.10.6")

	// GitLab-style: the fallback tag exists but is a single image manifest,
	// not an OCI index.
	fallbackBody := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":"sha256:%064d","size":2},"layers":[]}`,
		ociManifestMediaType, ociConfigMediaType, 0))
	srcReg.seedManifest("gitlab-org/build/cng/sidekiq-ee", referrersFallbackTag(manifestDigest),
		ociManifestMediaType, fallbackBody)

	_, dstHost := newFakeRegistryServer(t)
	mc := NewMirrorClient([]string{srcHost, dstHost}, "", dstHost)

	src := srcHost + "/gitlab-org/build/cng/sidekiq-ee:v18.10.6"
	dst := dstHost + "/operators/gitlab-org/build/cng/sidekiq-ee:v18.10.6"
	if _, err := mc.CopyImage(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyImage must not fail on a non-index referrers fallback tag: %v", err)
	}
	if exists, err := mc.CheckExist(context.Background(), dst); err != nil || !exists {
		t.Fatalf("destination image missing: exists=%v err=%v", exists, err)
	}
}

// TestCopyImage_CopiesReferrersBestEffort verifies that when the fallback tag
// is a valid OCI index, the listed referrers are copied to the destination.
func TestCopyImage_CopiesReferrersBestEffort(t *testing.T) {
	srcReg, srcHost := newFakeRegistryServer(t)
	manifestDigest, _ := seedTestImage(t, srcReg, "img", "v1")

	// Seed a referrer (e.g. a signature manifest) by digest and list it in the
	// fallback tag as an OCI index. Its config blob is the empty JSON object.
	configDigest := srcReg.seedBlob([]byte("{}"))
	refBody := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"artifactType":"application/vnd.dev.cosign.simplesigning.v1+json",`+
			`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},`+
			`"subject":{"mediaType":%q,"digest":%q,"size":700}}`,
		ociManifestMediaType, configDigest, ociManifestMediaType, manifestDigest))
	refDigest := srcReg.seedManifest("img", "sha256:1111111111111111111111111111111111111111111111111111111111111111", ociManifestMediaType, refBody)

	indexBody := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,`+
			`"artifactType":"application/vnd.dev.cosign.simplesigning.v1+json"}]}`,
		ociIndexMediaType, ociManifestMediaType, refDigest, len(refBody)))
	srcReg.seedManifest("img", referrersFallbackTag(manifestDigest), ociIndexMediaType, indexBody)

	_, dstHost := newFakeRegistryServer(t)
	mc := NewMirrorClient([]string{srcHost, dstHost}, "", dstHost)

	src := srcHost + "/img:v1"
	dst := dstHost + "/mirror/img:v1"
	if _, err := mc.CopyImage(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyImage: %v", err)
	}
	dstRef := dstHost + "/mirror/img@" + refDigest
	if exists, err := mc.CheckExist(context.Background(), dstRef); err != nil || !exists {
		t.Fatalf("referrer was not copied to destination: exists=%v err=%v", exists, err)
	}
}
