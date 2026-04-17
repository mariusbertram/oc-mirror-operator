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
	"fmt"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const (
	// EnvSourceCatalog is the env var consumed by catalog-builder: the source catalog image.
	EnvSourceCatalog = "SOURCE_CATALOG"
	// EnvTargetRef is the env var consumed by catalog-builder: the target OCI reference.
	EnvTargetRef = "TARGET_REF"
	// EnvCatalogPackages is the env var consumed by catalog-builder: comma-separated package names.
	EnvCatalogPackages = "CATALOG_PACKAGES"
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

// New returns a CatalogBuildManager.  The operator image is taken from the
// OPERATOR_IMAGE env var; if unset, "controller:latest" is used as a local
// development fallback.
func New() *CatalogBuildManager {
	img := os.Getenv(OperatorImageEnvVar)
	if img == "" {
		img = "controller:latest"
	}
	return &CatalogBuildManager{operatorImage: img}
}

// JobName returns a deterministic, DNS-safe Job name for the given ImageSet
// name and source catalog image reference.
func JobName(imageSetName, sourceCatalog string) string {
	slug := strings.ToLower(sourceCatalog)
	slug = strings.NewReplacer(":", "-", "/", "-", ".", "-", "@", "-").Replace(slug)
	if len(slug) > 40 {
		slug = slug[len(slug)-40:]
	}
	name := "catalog-build-" + imageSetName + "-" + slug
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
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
	packages []string,
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
	packages []string,
) *batchv1.Job {
	backoffLimit := int32(3)
	ttlAfterFinished := int32(600) // 10 min

	commonLabels := map[string]string{
		"app.kubernetes.io/managed-by": "oc-mirror-operator",
		"mirror.openshift.io/imageset": is.Name,
	}

	env := []corev1.EnvVar{
		{Name: EnvSourceCatalog, Value: sourceCatalog},
		{Name: EnvTargetRef, Value: targetRef},
		{Name: EnvCatalogPackages, Value: strings.Join(packages, ",")},
	}

	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if mt.Spec.AuthSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mt.Spec.AuthSecret,
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
					Containers: []corev1.Container{
						{
							Name:         "catalog-builder",
							Image:        m.operatorImage,
							Command:      []string{catalogBuilderBin},
							Env:          env,
							Resources:    mt.Spec.Worker.Resources,
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}
