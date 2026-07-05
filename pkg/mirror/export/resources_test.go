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
	"strings"
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
)

func TestRenderResources(t *testing.T) {
	spec := &mirrorv1alpha1.ImageSetSpec{
		Mirror: mirrorv1alpha1.Mirror{
			Operators: []mirrorv1alpha1.Operator{
				{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.14"},
			},
		},
	}
	manifest := Manifest{Images: []ManifestEntry{
		{
			Source:      "quay.io/foo/bar@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Destination: "registry.example.com/mirror/foo/bar",
			Origin:      imagestate.OriginRelease,
		},
	}}

	out, err := RenderResources("my-export", "my-namespace", spec, manifest, "registry.example.com/mirror")
	if err != nil {
		t.Fatalf("RenderResources() error = %v", err)
	}

	slug := resources.CatalogSlug(spec.Mirror.Operators[0].Catalog)
	csKey := "catalogsource-" + slug + ".yaml"
	ccKey := "clustercatalog-" + slug + ".yaml"

	for _, key := range []string{"idms.yaml", "itms.yaml", csKey, ccKey} {
		if _, ok := out[key]; !ok {
			t.Errorf("missing rendered artifact %q; got keys %v", key, keysOf(out))
		}
	}

	if !strings.Contains(string(out["idms.yaml"]), "quay.io/foo/bar") {
		t.Errorf("idms.yaml does not reference source image: %s", out["idms.yaml"])
	}
	if !strings.Contains(string(out[csKey]), "registry.example.com/mirror/redhat/redhat-operator-index") {
		t.Errorf("catalogsource yaml does not reference target catalog: %s", out[csKey])
	}
}

func TestRenderResources_NoOperators(t *testing.T) {
	spec := &mirrorv1alpha1.ImageSetSpec{}
	manifest := Manifest{}

	out, err := RenderResources("my-export", "my-namespace", spec, manifest, "registry.example.com/mirror")
	if err != nil {
		t.Fatalf("RenderResources() error = %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected only idms.yaml/itms.yaml with no operators, got keys %v", keysOf(out))
	}
}

func keysOf(m Resources) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
