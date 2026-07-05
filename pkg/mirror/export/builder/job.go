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

// Package builder creates and tracks the Kubernetes Job that resolves a
// MirrorExport's content and renders it into its artifacts ConfigMap. The Job
// runs the export-builder binary (shipped in the operator/controller image,
// like catalog-builder), which needs registry read access to the source
// content and Kubernetes API write access limited to a single named
// ConfigMap — see internal/controller/mirrorexport_controller.go for the
// accompanying ServiceAccount/Role/RoleBinding.
package builder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
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
	// EnvMirrorSpec is the env var consumed by export-builder: JSON-encoded
	// mirrorv1alpha1.Mirror (the content to resolve).
	EnvMirrorSpec = "MIRROR_SPEC"
	// EnvDestRegistry is the env var consumed by export-builder: the
	// destination registry destination refs are computed against.
	EnvDestRegistry = "DEST_REGISTRY"
	// EnvExportName is the env var consumed by export-builder: the
	// MirrorExport's name, used for IDMS/ITMS/CatalogSource object naming.
	EnvExportName = "EXPORT_NAME"
	// EnvArtifactsConfigMap is the env var consumed by export-builder: the
	// name of the (already-existing, operator-owned) ConfigMap to write
	// rendered artifacts into.
	EnvArtifactsConfigMap = "ARTIFACTS_CONFIGMAP"
	// EnvInsecureHosts is the env var consumed by export-builder:
	// comma-separated insecure registry hosts.
	EnvInsecureHosts = "REGISTRY_INSECURE_HOSTS"
	// EnvDockerConfig is the env var that points to the directory containing .dockerconfigjson.
	EnvDockerConfig = "DOCKER_CONFIG"
	// EnvPodNamespace is the downward-API env var: the namespace the
	// ConfigMap referenced by EnvArtifactsConfigMap lives in.
	EnvPodNamespace = "POD_NAMESPACE"

	// OperatorImageEnvVar is set in the operator's own Pod and tells the
	// ExportBuildManager which image to use for the export-builder Job.
	OperatorImageEnvVar = "OPERATOR_IMAGE"

	exportBuilderBin = "/export-builder"
)

// JobPhase represents the current lifecycle phase of an export build Job.
type JobPhase string

const (
	JobPhasePending   JobPhase = "Pending"
	JobPhaseRunning   JobPhase = "Running"
	JobPhaseSucceeded JobPhase = "Succeeded"
	JobPhaseFailed    JobPhase = "Failed"
	JobPhaseNotFound  JobPhase = "NotFound"
)

// ExportBuildManager creates and inspects the Kubernetes Job that resolves a
// MirrorExport's content.
type ExportBuildManager struct {
	operatorImage string
}

// New returns an ExportBuildManager. The operator image MUST be set via the
// OPERATOR_IMAGE env var; otherwise an error is returned.
func New() (*ExportBuildManager, error) {
	img := os.Getenv(OperatorImageEnvVar)
	if img == "" {
		return nil, fmt.Errorf("environment variable %s is not set", OperatorImageEnvVar)
	}
	return &ExportBuildManager{operatorImage: img}, nil
}

// JobName returns a deterministic, DNS-safe Job name for the given
// MirrorExport name.
func JobName(exportName string) string {
	return safeJobName("export-build", exportName)
}

// safeJobName builds a DNS-1123 compliant Job name (max 63 chars) by
// concatenating prefix and parts and appending an 8-char SHA-256 suffix
// derived from the *raw* parts, mirroring
// pkg/mirror/catalog/builder.safeJobName.
func safeJobName(prefix string, parts ...string) string {
	const maxLen = 63
	const sumLen = 8
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
	budget := maxLen - 1 - sumLen
	if len(body) > budget {
		body = body[:budget]
	}
	body = strings.TrimRight(body, "-")
	return body + "-" + suffix
}

// EnsureExportJob creates a Job that resolves me's content and writes the
// rendered artifacts into artifactsConfigMap. If a Job with the same name
// already exists the call is a no-op.
func (m *ExportBuildManager) EnsureExportJob(
	ctx context.Context,
	c client.Client,
	me *mirrorv1alpha1.MirrorExport,
	serviceAccountName, artifactsConfigMap, mirrorSpecJSON string,
) error {
	name := JobName(me.Name)

	existing := &batchv1.Job{}
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: me.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing export build Job %s: %w", name, err)
	}

	job := m.buildJobSpec(name, me, serviceAccountName, artifactsConfigMap, mirrorSpecJSON)
	if createErr := c.Create(ctx, job); createErr != nil {
		return fmt.Errorf("failed to create export build Job %s: %w", name, createErr)
	}
	return nil
}

// GetExportJobStatus returns the current phase of the named export build Job.
func GetExportJobStatus(ctx context.Context, c client.Client, name, namespace string) (JobPhase, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			return JobPhaseNotFound, nil
		}
		return "", fmt.Errorf("failed to get export build Job %s: %w", name, err)
	}

	switch {
	case job.Status.Succeeded > 0:
		return JobPhaseSucceeded, nil
	case job.Status.Active > 0:
		return JobPhaseRunning, nil
	case job.Status.Failed > 0:
		return JobPhaseFailed, nil
	default:
		return JobPhasePending, nil
	}
}

// DeleteExportJob deletes an export build Job so it can be recreated.
func DeleteExportJob(ctx context.Context, c client.Client, name, namespace string) error {
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

// Signature computes a deterministic hash of me's content-affecting spec
// fields. When this changes, the export must be re-rendered.
func Signature(me *mirrorv1alpha1.MirrorExport, mirrorSpecJSON string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "mirror=%s\n", mirrorSpecJSON)
	_, _ = fmt.Fprintf(h, "dest.registry=%s\n", me.Spec.Destination.Registry)
	_, _ = fmt.Fprintf(h, "dest.insecure=%t\n", me.Spec.Destination.Insecure)
	if src := me.Spec.Source; src != nil {
		_, _ = fmt.Fprintf(h, "src.registry=%s\n", src.Registry)
		_, _ = fmt.Fprintf(h, "src.insecure=%t\n", src.Insecure)
		_, _ = fmt.Fprintf(h, "src.authSecret=%s\n", src.AuthSecret)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// buildJobSpec constructs the batchv1.Job for an export build.
func (m *ExportBuildManager) buildJobSpec(
	name string,
	me *mirrorv1alpha1.MirrorExport,
	serviceAccountName, artifactsConfigMap, mirrorSpecJSON string,
) *batchv1.Job {
	backoffLimit := int32(3)
	ttlAfterFinished := int32(600)
	activeDeadlineSeconds := int64(1800)

	commonLabels := map[string]string{
		"app.kubernetes.io/managed-by": "oc-mirror-operator",
		"mirror.openshift.io/export":   me.Name,
	}

	env := []corev1.EnvVar{
		{Name: EnvMirrorSpec, Value: mirrorSpecJSON},
		{Name: EnvDestRegistry, Value: me.Spec.Destination.Registry},
		{Name: EnvExportName, Value: me.Name},
		{Name: EnvArtifactsConfigMap, Value: artifactsConfigMap},
		{Name: EnvPodNamespace, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
	}

	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

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

	var insecureHosts []string
	src := me.Spec.Source
	if src != nil {
		if src.Insecure && src.Registry != "" {
			insecureHosts = append(insecureHosts, hostOnly(src.Registry))
		}
		if src.AuthSecret != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "registry-auth",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: src.AuthSecret,
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
			env = append(env, corev1.EnvVar{
				Name:  EnvDockerConfig,
				Value: "/var/run/secrets/registry",
			})
		}
		if src.CABundle != nil {
			caKey := src.CABundle.Key
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
							Name: src.CABundle.ConfigMapName,
						},
						Items: []corev1.KeyToPath{
							{Key: caKey, Path: caKey},
						},
					},
				},
			})
		}
	}
	if len(insecureHosts) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  EnvInsecureHosts,
			Value: strings.Join(insecureHosts, ","),
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: me.Namespace,
			Labels:    commonLabels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(me, mirrorv1alpha1.GroupVersion.WithKind("MirrorExport")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlAfterFinished,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: commonLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: serviceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptrBool(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "export-builder",
							Image:   m.operatorImage,
							Command: []string{exportBuilderBin},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptrBool(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env:             env,
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

func resourcePtr(q string) *resource.Quantity {
	v := resource.MustParse(q)
	return &v
}

// hostOnly extracts the host[:port] prefix from a registry/repo path.
func hostOnly(registry string) string {
	if idx := strings.Index(registry, "/"); idx >= 0 {
		return registry[:idx]
	}
	return registry
}
