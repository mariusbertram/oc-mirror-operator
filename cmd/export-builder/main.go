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

// export-builder is a short-lived binary that is executed inside a
// Kubernetes Job by MirrorExportReconciler. It resolves a MirrorExport's
// content (the same resolution pkg/mirror/collector.go performs for a live
// MirrorTarget/ImageSet) and writes the rendered artifacts — a resolved
// image manifest, IDMS/ITMS/CatalogSource/ClusterCatalog resources, and a
// build spec for content that must be built rather than copied (operator
// catalog overlays, the Cincinnati graph-data image) — into an
// already-existing, operator-owned ConfigMap.
//
// Unlike catalog-builder, export-builder needs Kubernetes API access (to
// write that ConfigMap) in addition to registry read access — see
// internal/controller/mirrorexport_controller.go for the scoped
// ServiceAccount/Role/RoleBinding this Job runs as.
//
// Environment variables:
//
//	MIRROR_SPEC              – JSON-encoded api/v1alpha1.Mirror content spec (required)
//	DEST_REGISTRY            – destination registry destination refs are computed against (required)
//	EXPORT_NAME              – MirrorExport name, used for IDMS/ITMS/CatalogSource naming (required)
//	ARTIFACTS_CONFIGMAP      – name of the ConfigMap to write rendered artifacts into (required)
//	POD_NAMESPACE            – namespace of that ConfigMap (required, set via Downward API)
//	REGISTRY_INSECURE_HOSTS  – comma-separated list of insecure registry hosts
//	DOCKER_CONFIG            – directory that contains a .dockerconfigjson for source registry auth
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/export"
	builderconst "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/export/builder"
)

func main() {
	mirrorSpecRaw := os.Getenv(builderconst.EnvMirrorSpec)
	destRegistry := os.Getenv(builderconst.EnvDestRegistry)
	exportName := os.Getenv(builderconst.EnvExportName)
	artifactsConfigMap := os.Getenv(builderconst.EnvArtifactsConfigMap)
	namespace := os.Getenv(builderconst.EnvPodNamespace)
	insecureRaw := os.Getenv(builderconst.EnvInsecureHosts)
	authDir := os.Getenv(builderconst.EnvDockerConfig)

	if mirrorSpecRaw == "" || destRegistry == "" || exportName == "" || artifactsConfigMap == "" || namespace == "" {
		slog.Error("MIRROR_SPEC, DEST_REGISTRY, EXPORT_NAME, ARTIFACTS_CONFIGMAP, and POD_NAMESPACE environment variables are required")
		os.Exit(1)
	}

	var mirrorSpec mirrorv1alpha1.Mirror
	if err := json.Unmarshal([]byte(mirrorSpecRaw), &mirrorSpec); err != nil {
		slog.Error("failed to parse MIRROR_SPEC", "error", err)
		os.Exit(1)
	}

	var insecureHosts []string
	for _, h := range strings.Split(insecureRaw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			insecureHosts = append(insecureHosts, h)
		}
	}

	startTime := time.Now()
	slog.Info("starting export-builder",
		"exportName", exportName,
		"destRegistry", destRegistry,
		"artifactsConfigMap", artifactsConfigMap,
		"namespace", namespace,
		"insecureHosts", insecureHosts,
		"startTime", startTime.Format(time.RFC3339),
	)

	mc := mirrorclient.NewMirrorClient(insecureHosts, authDir)
	collector := mirror.NewCollector(mc)
	spec := &mirrorv1alpha1.ImageSetSpec{Mirror: mirrorSpec}

	// Generous timeout: resolution reads Cincinnati graphs, catalog FBC
	// layers, and Helm charts, none of which involve pushing image blobs, but
	// large operator catalogs can still take several minutes to parse.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	manifest, err := export.BuildManifest(ctx, collector, spec, destRegistry)
	if err != nil {
		slog.Error("failed to resolve content", "error", err)
		os.Exit(1)
	}

	buildSpec := export.BuildSpecFor(spec, destRegistry)

	rendered, err := export.RenderResources(exportName, namespace, spec, manifest, destRegistry)
	if err != nil {
		slog.Error("failed to render resources", "error", err)
		os.Exit(1)
	}

	data := make(map[string]string, len(rendered)+2)
	for k, v := range rendered {
		data[k] = string(v)
	}

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		slog.Error("failed to marshal manifest", "error", err)
		os.Exit(1)
	}
	data["manifest.json"] = string(manifestJSON)

	buildSpecJSON, err := json.MarshalIndent(buildSpec, "", "  ")
	if err != nil {
		slog.Error("failed to marshal build spec", "error", err)
		os.Exit(1)
	}
	data["buildspec.json"] = string(buildSpecJSON)

	if err := writeArtifacts(ctx, namespace, artifactsConfigMap, data); err != nil {
		slog.Error("failed to write artifacts ConfigMap", "error", err)
		os.Exit(1)
	}

	elapsed := time.Since(startTime)
	slog.Info("export-builder finished successfully",
		"images", len(manifest.Images),
		"catalogBuilds", len(buildSpec.Catalogs),
		"elapsed", elapsed.Round(time.Millisecond).String(),
	)
}

// writeArtifacts patches the already-existing artifacts ConfigMap (created
// and owned by MirrorExportReconciler so garbage collection and RBAC
// resourceNames scoping both work) with the rendered data.
//
// NOTE: ConfigMaps are limited to ~1MiB total. Very large manifests (many
// thousands of images) are not yet handled — see imagestate's gzip-compressed
// ConfigMap approach for the pattern to adopt if this becomes a problem.
func writeArtifacts(ctx context.Context, namespace, name string, data map[string]string) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get ConfigMap %s/%s: %w", namespace, name, err)
	}
	cm.Data = data
	if _, err := clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ConfigMap %s/%s: %w", namespace, name, err)
	}
	return nil
}
