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
	"fmt"
	"strings"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// CatalogBuildEntry carries everything the (not-yet-implemented, see
// issue #82 rollout step 2) catalog/graph-builder tool needs to build one
// filtered operator catalog overlay — the same inputs cmd/catalog-builder
// already takes as SOURCE_CATALOG/TARGET_REF/CATALOG_INCLUDE_CONFIG env vars.
// Building the overlay itself is out of scope here: unlike every other
// image in the manifest, the filtered catalog does not exist upstream as a
// copyable reference, so it cannot be resolved — only planned.
type CatalogBuildEntry struct {
	// SourceCatalog is the unfiltered upstream catalog image reference.
	SourceCatalog string `json:"sourceCatalog"`
	// TargetRef is the destination reference the built overlay must be
	// pushed to, computed against Spec.Destination.Registry.
	TargetRef string `json:"targetRef"`
	// Full mirrors Operator.Full: when true, Packages is empty and every
	// package in the catalog should be included unfiltered.
	Full bool `json:"full,omitempty"`
	// Packages mirrors Operator.Packages (channel/version filters). Empty
	// when Full is true.
	Packages []mirrorv1alpha1.IncludePackage `json:"packages,omitempty"`
}

// GraphBuildSpec describes whether the Cincinnati graph-data image
// (pkg/mirror/graph) needs to be built. The graph-data archive is a single
// upstream artifact independent of any channel selection, so there is
// nothing else to plan beyond the flag itself.
type GraphBuildSpec struct {
	Enabled bool `json:"enabled"`
	// TargetRef is the destination reference the built graph-data image must
	// be pushed to, computed against Spec.Destination.Registry.
	TargetRef string `json:"targetRef,omitempty"`
}

// BuildSpec is the data-only description of everything that must be *built*
// (not merely copied) to complete a MirrorExport: the filtered operator
// catalog overlay per Mirror.Operators[] entry, and the graph-data image
// when Mirror.Platform.Graph is set.
type BuildSpec struct {
	Catalogs []CatalogBuildEntry `json:"catalogs,omitempty"`
	Graph    *GraphBuildSpec     `json:"graph,omitempty"`
}

// BuildSpecFor derives a BuildSpec from spec — pure data transformation, no
// registry access, mirroring catalogTargetRef in
// internal/controller/imageset_controller.go and graph.TargetImage.
func BuildSpecFor(spec *mirrorv1alpha1.ImageSetSpec, destRegistry string) BuildSpec {
	var bs BuildSpec

	for _, op := range spec.Mirror.Operators {
		if op.Catalog == "" {
			continue
		}
		var packages []mirrorv1alpha1.IncludePackage
		if !op.Full {
			packages = op.Packages
		}
		bs.Catalogs = append(bs.Catalogs, CatalogBuildEntry{
			SourceCatalog: op.Catalog,
			TargetRef:     catalogTargetRef(destRegistry, op),
			Full:          op.Full,
			Packages:      packages,
		})
	}

	if spec.Mirror.Platform.Graph {
		bs.Graph = &GraphBuildSpec{
			Enabled:   true,
			TargetRef: fmt.Sprintf("%s/openshift/graph-image:latest", destRegistry),
		}
	}

	return bs
}

// catalogTargetRef builds the target image reference for a filtered catalog
// image. Duplicated (rather than imported) from
// internal/controller/imageset_controller.go's unexported helper of the same
// name and behavior, to keep pkg/mirror/export independent of the internal/
// controller package.
func catalogTargetRef(registry string, op mirrorv1alpha1.Operator) string {
	tag := op.TargetTag
	if tag == "" {
		catalogForTag := op.Catalog
		if i := strings.Index(catalogForTag, "@"); i >= 0 {
			catalogForTag = catalogForTag[:i]
		}
		if i := strings.LastIndex(catalogForTag, ":"); i >= 0 && !strings.Contains(catalogForTag[i:], "/") {
			tag = catalogForTag[i+1:]
		}
	}
	if tag == "" {
		tag = "latest"
	}
	if op.TargetCatalog != "" {
		return fmt.Sprintf("%s/%s:%s", registry, op.TargetCatalog, tag)
	}
	parts := strings.SplitN(op.Catalog, "/", 2)
	path := op.Catalog
	if len(parts) == 2 {
		path = parts[1]
	}
	if i := strings.IndexAny(path, ":@"); i >= 0 {
		path = path[:i]
	}
	return fmt.Sprintf("%s/%s:%s", registry, path, tag)
}
