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

package export

import (
	"context"
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// TestBuildManifest_AdditionalImages exercises BuildManifest end-to-end using
// only spec.mirror.additionalImages, since resolving those needs no registry
// I/O (unlike releases/operators/helm, already covered by
// pkg/mirror/collector_test.go) — this still validates BuildManifest's own
// logic: origin tagging and blocked-image filtering across collector calls.
func TestBuildManifest_AdditionalImages(t *testing.T) {
	spec := &mirrorv1alpha1.ImageSetSpec{
		Mirror: mirrorv1alpha1.Mirror{
			AdditionalImages: []mirrorv1alpha1.AdditionalImage{
				{Name: "quay.io/foo/bar:v1"},
				{Name: "quay.io/foo/blocked:v1"},
			},
			BlockedImages: []mirrorv1alpha1.BlockedImage{
				{Name: "foo/blocked"},
			},
		},
	}

	c := mirror.NewCollector(mirrorclient.NewMirrorClient(nil, ""))
	manifest, err := BuildManifest(context.Background(), c, spec, "registry.example.com/mirror")
	if err != nil {
		t.Fatalf("BuildManifest() error = %v", err)
	}

	if len(manifest.Images) != 1 {
		t.Fatalf("Images = %d, want 1 (blocked image must be filtered)", len(manifest.Images))
	}
	got := manifest.Images[0]
	if got.Source != "quay.io/foo/bar:v1" {
		t.Errorf("Source = %q, want quay.io/foo/bar:v1", got.Source)
	}
	if got.Destination != "registry.example.com/mirror/quay.io/foo/bar:v1" {
		t.Errorf("Destination = %q, want registry.example.com/mirror/quay.io/foo/bar:v1", got.Destination)
	}
	if got.Origin != imagestate.OriginAdditional {
		t.Errorf("Origin = %q, want %q", got.Origin, imagestate.OriginAdditional)
	}
}

func TestTagOrigin(t *testing.T) {
	images := []mirror.TargetImage{
		{Source: "a", Destination: "b", BundleRef: "bundle-a"},
	}
	entries := tagOrigin(images, imagestate.OriginOperator)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Origin != imagestate.OriginOperator {
		t.Errorf("Origin = %q, want %q", entries[0].Origin, imagestate.OriginOperator)
	}
	if entries[0].BundleRef != "bundle-a" {
		t.Errorf("BundleRef = %q, want bundle-a", entries[0].BundleRef)
	}
}

func TestBlockEntries(t *testing.T) {
	entries := []ManifestEntry{
		{Source: "registry.io/foo/bar:v1"},
		{Source: "registry.io/foo/baz@sha256:abc"},
	}
	blocked := []mirrorv1alpha1.BlockedImage{{Name: "foo/baz"}}

	got := blockEntries(entries, blocked)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Source != "registry.io/foo/bar:v1" {
		t.Errorf("Source = %q, want registry.io/foo/bar:v1", got[0].Source)
	}

	// No blocked images: input returned unchanged (same behavior as mirror.BlockImages).
	if got := blockEntries(entries, nil); len(got) != len(entries) {
		t.Errorf("no blocklist: entries = %d, want %d", len(got), len(entries))
	}
}

func TestManifestToImageState(t *testing.T) {
	m := Manifest{Images: []ManifestEntry{
		{Source: "quay.io/foo/bar@sha256:abc", Destination: "registry.example.com/mirror/foo/bar", Origin: imagestate.OriginRelease},
	}}

	state := m.ToImageState("my-export")
	entry, ok := state["registry.example.com/mirror/foo/bar"]
	if !ok {
		t.Fatalf("destination not present in ImageState")
	}
	if entry.Source != "quay.io/foo/bar@sha256:abc" {
		t.Errorf("Source = %q, want quay.io/foo/bar@sha256:abc", entry.Source)
	}
	if entry.State != "Mirrored" {
		t.Errorf("State = %q, want Mirrored", entry.State)
	}
	if !entry.HasImageSet("my-export") {
		t.Errorf("expected Refs to include ImageSet %q", "my-export")
	}
}
