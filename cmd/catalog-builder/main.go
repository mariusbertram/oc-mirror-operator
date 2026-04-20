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

// catalog-builder is a short-lived binary that is executed inside a Kubernetes
// Job.  It reads its configuration from environment variables, pulls the source
// catalog, filters the FBC to the requested packages, and pushes the resulting
// OCI catalog image to the target registry.
//
// Environment variables:
//
//	SOURCE_CATALOG           – fully-qualified source catalog image reference (required)
//	TARGET_REF               – fully-qualified target image reference (required)
//	CATALOG_PACKAGES         – comma-separated list of package names to include (empty = all)
//	REGISTRY_INSECURE_HOSTS  – comma-separated list of insecure registry hosts
//	DOCKER_CONFIG            – directory that contains a .dockerconfigjson for registry auth
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

func main() {
	source := os.Getenv("SOURCE_CATALOG")
	target := os.Getenv("TARGET_REF")
	pkgsRaw := os.Getenv("CATALOG_PACKAGES")
	insecureRaw := os.Getenv("REGISTRY_INSECURE_HOSTS")

	if source == "" || target == "" {
		slog.Error("SOURCE_CATALOG and TARGET_REF environment variables are required")
		os.Exit(1)
	}

	var packages []string
	for _, p := range strings.Split(pkgsRaw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			packages = append(packages, p)
		}
	}

	var insecureHosts []string
	for _, h := range strings.Split(insecureRaw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			insecureHosts = append(insecureHosts, h)
		}
	}

	authDir := os.Getenv("DOCKER_CONFIG")

	slog.Info("starting catalog-builder",
		"source", source,
		"target", target,
		"packages", packages,
		"insecureHosts", insecureHosts,
		"authDir", authDir,
	)

	mc := mirrorclient.NewMirrorClient(insecureHosts, authDir)
	resolver := catalog.New(mc)

	ctx := context.Background()
	digest, err := resolver.BuildFilteredCatalogImage(ctx, source, target, packages)
	if err != nil {
		slog.Error("failed to build filtered catalog image", "error", err)
		os.Exit(1)
	}

	slog.Info("catalog image pushed successfully", "digest", digest, "target", target)
}
