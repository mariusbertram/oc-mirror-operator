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

// Package builder creates and tracks Kubernetes Jobs that build filtered OLM
// catalog images.  Each Job runs the catalog-builder binary (shipped in the
// same operator image) which pulls the source catalog, filters the FBC to the
// requested packages, and pushes the result to the target registry.
package builder

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const (
	// EnvSourceCatalog is the env var consumed by catalog-builder: the source catalog image.
	EnvSourceCatalog = "SOURCE_CATALOG"
	// EnvTargetRef is the env var consumed by catalog-builder: the target OCI reference.
	EnvTargetRef = "TARGET_REF"
	// EnvCatalogPackages is the env var consumed by catalog-builder: comma-separated package names (legacy/fallback).
	EnvCatalogPackages = "CATALOG_PACKAGES"
	// EnvCatalogIncludeConfig is the env var consumed by catalog-builder: JSON-encoded []IncludePackage
	// with channel and version filter details. Takes priority over CATALOG_PACKAGES when present.
	EnvCatalogIncludeConfig = "CATALOG_INCLUDE_CONFIG"
	// EnvInsecureHosts is the env var consumed by catalog-builder: comma-separated insecure registry hosts.
	EnvInsecureHosts = "REGISTRY_INSECURE_HOSTS"
	// EnvDockerConfig is the env var that points to the directory containing .dockerconfigjson.
	EnvDockerConfig = "DOCKER_CONFIG"

	// OperatorImageEnvVar is set in the operator's own Pod and tells the
	// CatalogBuildManager which image to use for builder Jobs.
	OperatorImageEnvVar = "OPERATOR_IMAGE"

	catalogBuilderBin = "/catalog-builder"
)

// JobPhase represents the current lifecycle phase of a catalog build Job.
type JobPhase string

const (
	JobPhasePending   JobPhase = "Pending"
	JobPhaseRunning   JobPhase = "Running"
	JobPhaseSucceeded JobPhase = "Succeeded"
	JobPhaseFailed    JobPhase = "Failed"
	JobPhaseNotFound  JobPhase = "NotFound"
)

// CatalogBuildManager creates and inspects Kubernetes Jobs that build filtered
// catalog images.
type CatalogBuildManager struct {
	operatorImage string
}

// New returns a CatalogBuildManager. The operator image MUST be set via the
// OPERATOR_IMAGE env var; otherwise an error is returned. The operator's main
// entrypoint is expected to validate this at startup so that catalog builds
// always reference a deterministic, content-addressable image.
func New() (*CatalogBuildManager, error) {
	img := os.Getenv(OperatorImageEnvVar)
	if img == "" {
		return nil, fmt.Errorf("environment variable %s is not set", OperatorImageEnvVar)
	}
	return &CatalogBuildManager{operatorImage: img}, nil
}

// MustNew is the panicking variant of New, intended for use after the
// operator's main entrypoint has already validated OPERATOR_IMAGE.
func MustNew() *CatalogBuildManager {
	m, err := New()
	if err != nil {
		panic(err)
	}
	return m
}

// JobName returns a deterministic, DNS-safe Job name for the given ImageSet
// name and source catalog image reference. To avoid collisions when long names
// get truncated, the final 8 characters are derived from a SHA-256 of all
// inputs.
func JobName(imageSetName, sourceCatalog string) string {
	return safeJobName("catalog-build", imageSetName, sourceCatalog)
}

// safeJobName builds a DNS-1123 compliant Job name (max 63 chars) by
// concatenating prefix and parts and appending an 8-char SHA-256 suffix
// derived from the *raw* parts. This guarantees that two different inputs
// cannot collide via truncation.
func safeJobName(prefix string, parts ...string) string { //nolint:unparam
	const maxLen = 63
	const sumLen = 8 // hex chars
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	suffix := fmt.Sprintf("%x", h.Sum(nil))[:sumLen]

	slugParts := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.ToLower(p)
		s = strings.NewReplacer(":", "-", "/", "-", ".", "-", "@", "-", "_", "-").Replace(s)
		slugParts = append(slugParts, s)
	}
	body := prefix + "-" + strings.Join(slugParts, "-")
	// Reserve room for "-<suffix>"
	budget := maxLen - 1 - sumLen
	if len(body) > budget {
		body = body[:budget]
	}
	body = strings.TrimRight(body, "-")
	return body + "-" + suffix
}

// EnsureCatalogBuildJob creates a Job that builds the filtered catalog image
// from sourceCatalog and pushes the result to targetRef.  If a Job with the
// same name already exists the call is a no-op.
func (m *CatalogBuildManager) EnsureCatalogBuildJob(
	ctx context.Context,
	c client.Client,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	sourceCatalog string,
	targetRef string,
	packages []mirrorv1alpha1.IncludePackage,
) error {
	name := JobName(is.Name, sourceCatalog)

	existing := &batchv1.Job{}
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: is.Namespace}, existing)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing CatalogBuildJob %s: %w", name, err)
	}

	job := m.buildJobSpec(name, is, mt, sourceCatalog, targetRef, packages)
	if createErr := c.Create(ctx, job); createErr != nil {
		return fmt.Errorf("failed to create CatalogBuildJob %s: %w", name, createErr)
	}
	return nil
}

// GetBuildJobStatus returns the current phase of the named catalog build Job.
func GetBuildJobStatus(ctx context.Context, c client.Client, name, namespace string) (JobPhase, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			return JobPhaseNotFound, nil
		}
		return "", fmt.Errorf("failed to get CatalogBuildJob %s: %w", name, err)
	}

	switch {
	case job.Status.Succeeded > 0:
		return JobPhaseSucceeded, nil
	case job.Status.Failed > 0:
		return JobPhaseFailed, nil
	case job.Status.Active > 0:
		return JobPhaseRunning, nil
	default:
		return JobPhasePending, nil
	}
}

// buildJobSpec constructs the batchv1.Job for a catalog build.
func (m *CatalogBuildManager) buildJobSpec(
	name string,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	sourceCatalog, targetRef string,
	packages []mirrorv1alpha1.IncludePackage,
) *batchv1.Job {
	backoffLimit := int32(3)
	ttlAfterFinished := int32(600) // 10 min

	commonLabels := map[string]string{
		"app.kubernetes.io/managed-by": "oc-mirror-operator",
		"mirror.openshift.io/imageset": is.Name,
	}

	// Legacy package names for backward compatibility.
	pkgNames := make([]string, 0, len(packages))
	for _, p := range packages {
		pkgNames = append(pkgNames, p.Name)
	}

	env := []corev1.EnvVar{
		{Name: EnvSourceCatalog, Value: sourceCatalog},
		{Name: EnvTargetRef, Value: targetRef},
		{Name: EnvCatalogPackages, Value: strings.Join(pkgNames, ",")},
	}

	// Include the full filter config (channels + versions) when available.
	if len(packages) > 0 {
		if data, err := json.Marshal(packages); err == nil {
			env = append(env, corev1.EnvVar{Name: EnvCatalogIncludeConfig, Value: string(data)})
		}
	}

	if mt.Spec.Insecure {
		// Extract only the host:port from the registry path (strip any path prefix).
		registryHost := mt.Spec.Registry
		if idx := strings.Index(registryHost, "/"); idx >= 0 {
			registryHost = registryHost[:idx]
		}
		env = append(env, corev1.EnvVar{
			Name:  EnvInsecureHosts,
			Value: registryHost,
		})
	}

	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	// Blob buffer volume for large layer copies (used by bufferLargeBlobs hook).
	volumes = append(volumes, corev1.Volume{
		Name: "blob-buffer",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resourcePtr("10Gi"),
			},
		},
	})
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "blob-buffer",
		MountPath: "/tmp/blob-buffer",
	})

	if mt.Spec.AuthSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mt.Spec.AuthSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "registry-auth",
			MountPath: "/var/run/secrets/registry",
			ReadOnly:  true,
		})
		// Tell regclient (via the standard DOCKER_CONFIG env var) where to find credentials.
		env = append(env, corev1.EnvVar{
			Name:  EnvDockerConfig,
			Value: "/var/run/secrets/registry",
		})
	}

	// Inject proxy env vars when configured.
	env = append(env, catalogProxyEnvVars(mt.Spec.Proxy)...)

	// Inject CA bundle when configured.
	if mt.Spec.CABundle != nil {
		caKey := mt.Spec.CABundle.Key
		if caKey == "" {
			caKey = "ca-bundle.crt"
		}
		env = append(env, corev1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: "/run/secrets/ca/" + caKey,
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "ca-bundle",
			MountPath: "/run/secrets/ca",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "ca-bundle",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: mt.Spec.CABundle.ConfigMapName,
					},
					Items: []corev1.KeyToPath{
						{Key: caKey, Path: caKey},
					},
				},
			},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: is.Namespace,
			Labels:    commonLabels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(is, mirrorv1alpha1.GroupVersion.WithKind("ImageSet")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: commonLabels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  mt.Spec.Worker.NodeSelector,
					Tolerations:   mt.Spec.Worker.Tolerations,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptrBool(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "catalog-builder",
							Image:   m.operatorImage,
							Command: []string{catalogBuilderBin},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptrBool(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env:             env,
							Resources:       mt.Spec.Worker.Resources,
							VolumeMounts:    volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}

func ptrBool(b bool) *bool { return &b }

// resourcePtr parses a resource quantity string and returns a pointer to it.
// Used for EmptyDir size limits. Panics on invalid input (only invoked with constants).
func resourcePtr(q string) *resource.Quantity {
	v := resource.MustParse(q)
	return &v
}

// catalogProxyEnvVars returns HTTP/HTTPS/NO_PROXY environment variables (upper
// and lower case) for the catalog-builder job.  Returns nil when cfg is nil.
func catalogProxyEnvVars(cfg *mirrorv1alpha1.ProxyConfig) []corev1.EnvVar {
	if cfg == nil {
		return nil
	}
	var env []corev1.EnvVar
	if v := cfg.HTTPProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: v},
			corev1.EnvVar{Name: "http_proxy", Value: v},
		)
	}
	if v := cfg.HTTPSProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: v},
			corev1.EnvVar{Name: "https_proxy", Value: v},
		)
	}
	if v := cfg.NoProxy; v != "" {
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: v},
			corev1.EnvVar{Name: "no_proxy", Value: v},
		)
	}
	return env
}

// DeleteBuildJob deletes a catalog build Job so it can be recreated.
func DeleteBuildJob(ctx context.Context, c client.Client, name, namespace string) error {
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	propagation := metav1.DeletePropagationBackground
	return c.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}

// BuildSignature computes a deterministic hash of the operator image and
// package configuration including channel and version filter details.
// When this changes, catalog builds should be re-run.
func (m *CatalogBuildManager) BuildSignature(operators []mirrorv1alpha1.Operator) string {
	h := sha256.New()
	// Include the operator image so image upgrades force a rebuild.
	_, _ = fmt.Fprintf(h, "image=%s\n", m.operatorImage)

	for _, op := range operators {
		if op.Catalog == "" {
			continue
		}
		_, _ = fmt.Fprintf(h, "catalog=%s\n", op.Catalog)
		_, _ = fmt.Fprintf(h, "full=%t\n", op.Full)

		// Sort packages for deterministic output.
		pkgs := make([]mirrorv1alpha1.IncludePackage, len(op.Packages))
		copy(pkgs, op.Packages)
		sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })

		for _, p := range pkgs {
			_, _ = fmt.Fprintf(h, "pkg=%s\n", p.Name)
			if p.MinVersion != "" {
				_, _ = fmt.Fprintf(h, "pkg.min=%s\n", p.MinVersion)
			}
			if p.MaxVersion != "" {
				_, _ = fmt.Fprintf(h, "pkg.max=%s\n", p.MaxVersion)
			}
			// Sort channels for deterministic output.
			chans := make([]mirrorv1alpha1.IncludeChannel, len(p.Channels))
			copy(chans, p.Channels)
			sort.Slice(chans, func(i, j int) bool { return chans[i].Name < chans[j].Name })
			for _, ch := range chans {
				_, _ = fmt.Fprintf(h, "ch=%s\n", ch.Name)
				if ch.MinVersion != "" {
					_, _ = fmt.Fprintf(h, "ch.min=%s\n", ch.MinVersion)
				}
				if ch.MaxVersion != "" {
					_, _ = fmt.Fprintf(h, "ch.max=%s\n", ch.MaxVersion)
				}
			}
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}
