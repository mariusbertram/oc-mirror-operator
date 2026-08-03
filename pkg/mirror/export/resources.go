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

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
)

// Resources holds the rendered IDMS/ITMS/CatalogSource/ClusterCatalog YAML
// documents for a MirrorExport, keyed the same way the live resource server
// (pkg/resourceapi) keys them in its "<name>-resources" ConfigMap, so the
// artifact layout is familiar to existing MirrorTarget/ImageSet users:
// "idms.yaml", "itms.yaml", "catalogsource-<slug>.yaml", "clustercatalog-<slug>.yaml".
type Resources map[string][]byte

// RenderResources generates IDMS, ITMS, and per-operator-entry
// CatalogSource/ClusterCatalog YAML from the resolved manifest, reusing
// pkg/mirror/resources' generation logic unchanged (the same functions the
// live resource server calls for a connected MirrorTarget) against a
// synthetic imagestate.ImageState built from the manifest.
//
// name is used as the IDMS/ITMS object name and as the "<name>-<slug>"
// CatalogSource/ClusterCatalog object name, matching the manager's
// "<targetName>-<slug>" convention (pkg/mirror/manager/manager.go).
// namespace is used for CatalogSource's metadata.namespace — typically the
// MirrorExport's own namespace; edit it if the disconnected cluster's
// namespace differs before applying.
func RenderResources(name, namespace string, spec *mirrorv1alpha1.ImageSetSpec, manifest Manifest, destRegistry string) (Resources, error) {
	out := make(Resources)

	state := manifest.ToImageState(name)

	idms, err := resources.GenerateIDMS(name, state)
	if err != nil {
		return nil, fmt.Errorf("generate IDMS: %w", err)
	}
	out["idms.yaml"] = idms

	itms, err := resources.GenerateITMS(name, state)
	if err != nil {
		return nil, fmt.Errorf("generate ITMS: %w", err)
	}
	out["itms.yaml"] = itms

	for _, op := range spec.Mirror.Operators {
		if op.Catalog == "" {
			continue
		}
		slug := resources.CatalogSlug(op.Catalog)
		cat := resources.CatalogInfo{
			SourceCatalog: op.Catalog,
			TargetImage:   resources.CatalogTargetImage(destRegistry, op),
			DisplayName:   op.Catalog,
		}

		cs, err := resources.GenerateCatalogSource(name+"-"+slug, namespace, cat, "")
		if err != nil {
			return nil, fmt.Errorf("generate CatalogSource for %s: %w", op.Catalog, err)
		}
		out[fmt.Sprintf("catalogsource-%s.yaml", slug)] = cs

		cc, err := resources.GenerateClusterCatalog(name+"-"+slug, cat)
		if err != nil {
			return nil, fmt.Errorf("generate ClusterCatalog for %s: %w", op.Catalog, err)
		}
		out[fmt.Sprintf("clustercatalog-%s.yaml", slug)] = cc
	}

	return out, nil
}
