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
	"testing"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func TestBuildSpecFor(t *testing.T) {
	tests := []struct {
		name           string
		spec           mirrorv1alpha1.ImageSetSpec
		destRegistry   string
		wantCatalogs   int
		wantTargetRef  string
		wantFull       bool
		wantGraph      bool
		wantGraphImage string
	}{
		{
			name: "single filtered operator entry",
			spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{
							Catalog:       "registry.redhat.io/redhat/redhat-operator-index:v4.14",
							IncludeConfig: mirrorv1alpha1.IncludeConfig{Packages: []mirrorv1alpha1.IncludePackage{{Name: "web-terminal"}}},
						},
					},
				},
			},
			destRegistry:  "registry.example.com/mirror",
			wantCatalogs:  1,
			wantTargetRef: "registry.example.com/mirror/redhat/redhat-operator-index:v4.14",
		},
		{
			name: "full catalog entry drops packages",
			spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{
					Operators: []mirrorv1alpha1.Operator{
						{
							Catalog:       "registry.redhat.io/redhat/redhat-operator-index:v4.14",
							Full:          true,
							IncludeConfig: mirrorv1alpha1.IncludeConfig{Packages: []mirrorv1alpha1.IncludePackage{{Name: "web-terminal"}}},
						},
					},
				},
			},
			destRegistry: "registry.example.com/mirror",
			wantCatalogs: 1,
			wantFull:     true,
		},
		{
			name:         "entry without a catalog is skipped",
			spec:         mirrorv1alpha1.ImageSetSpec{Mirror: mirrorv1alpha1.Mirror{Operators: []mirrorv1alpha1.Operator{{}}}},
			destRegistry: "registry.example.com/mirror",
			wantCatalogs: 0,
		},
		{
			name: "graph enabled",
			spec: mirrorv1alpha1.ImageSetSpec{
				Mirror: mirrorv1alpha1.Mirror{Platform: mirrorv1alpha1.Platform{Graph: true}},
			},
			destRegistry:   "registry.example.com/mirror",
			wantGraph:      true,
			wantGraphImage: "registry.example.com/mirror/openshift/graph-image:latest",
		},
		{
			name:         "graph disabled",
			spec:         mirrorv1alpha1.ImageSetSpec{},
			destRegistry: "registry.example.com/mirror",
			wantGraph:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := BuildSpecFor(&tt.spec, tt.destRegistry)

			if len(bs.Catalogs) != tt.wantCatalogs {
				t.Fatalf("catalogs = %d, want %d", len(bs.Catalogs), tt.wantCatalogs)
			}
			if tt.wantCatalogs > 0 {
				got := bs.Catalogs[0]
				if tt.wantTargetRef != "" && got.TargetRef != tt.wantTargetRef {
					t.Errorf("TargetRef = %q, want %q", got.TargetRef, tt.wantTargetRef)
				}
				if got.Full != tt.wantFull {
					t.Errorf("Full = %v, want %v", got.Full, tt.wantFull)
				}
				if tt.wantFull && len(got.Packages) != 0 {
					t.Errorf("Packages = %v, want empty when Full", got.Packages)
				}
			}

			if tt.wantGraph {
				if bs.Graph == nil || !bs.Graph.Enabled {
					t.Fatalf("Graph = %+v, want enabled", bs.Graph)
				}
				if bs.Graph.TargetRef != tt.wantGraphImage {
					t.Errorf("Graph.TargetRef = %q, want %q", bs.Graph.TargetRef, tt.wantGraphImage)
				}
			} else if bs.Graph != nil {
				t.Errorf("Graph = %+v, want nil", bs.Graph)
			}
		})
	}
}

func TestCatalogTargetRef(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		op       mirrorv1alpha1.Operator
		want     string
	}{
		{
			name:     "derives path and tag from source catalog",
			registry: "registry.example.com/mirror",
			op:       mirrorv1alpha1.Operator{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.14"},
			want:     "registry.example.com/mirror/redhat/redhat-operator-index:v4.14",
		},
		{
			name:     "digest-only source defaults to latest",
			registry: "registry.example.com/mirror",
			op:       mirrorv1alpha1.Operator{Catalog: "registry.redhat.io/redhat/redhat-operator-index@sha256:abc"},
			want:     "registry.example.com/mirror/redhat/redhat-operator-index:latest",
		},
		{
			name:     "explicit TargetCatalog overrides derived path",
			registry: "registry.example.com/mirror",
			op:       mirrorv1alpha1.Operator{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.14", TargetCatalog: "custom/catalog"},
			want:     "registry.example.com/mirror/custom/catalog:v4.14",
		},
		{
			name:     "explicit TargetTag overrides derived tag",
			registry: "registry.example.com/mirror",
			op:       mirrorv1alpha1.Operator{Catalog: "registry.redhat.io/redhat/redhat-operator-index:v4.14", TargetTag: "custom-tag"},
			want:     "registry.example.com/mirror/redhat/redhat-operator-index:custom-tag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := catalogTargetRef(tt.registry, tt.op)
			if got != tt.want {
				t.Errorf("catalogTargetRef() = %q, want %q", got, tt.want)
			}
		})
	}
}
