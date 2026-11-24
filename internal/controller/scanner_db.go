package controller

import (
	"context"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func (r *MirrorTargetReconciler) reconcileScannerDB(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: mt.Name + "-acs-scanner-db", Namespace: mt.Namespace}}
	db := mt.Spec.ACSScannerDB
	if db == nil {
		if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(deploy, mt) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, deploy, client.Preconditions{UID: &deploy.UID}))
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if deploy.UID != "" && !metav1.IsControlledBy(deploy, mt) {
			return fmt.Errorf("deployment %s already exists with a different owner", deploy.Name)
		}
		if err := controllerutil.SetControllerReference(mt, deploy, r.Scheme); err != nil {
			return err
		}

		labels := map[string]string{"app": "oc-mirror-acs-scanner-db", "mirrortarget": mt.Name}
		args := []string{"acs-scanner-db", "--registry", mt.Spec.Registry, "--version", db.Version}
		if mt.Spec.Insecure {
			args = append(args, "--insecure")
		}
		storage := resource.MustParse("1Gi")
		if db.Version == "" {
			storage = resource.MustParse("2Gi")
		}

		deploy.Labels = labels
		deploy.Spec.Replicas = pointerTo(int32(1))
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		deploy.Spec.Template.Labels = labels
		pod := &deploy.Spec.Template.Spec
		pod.AutomountServiceAccountToken = pointerTo(false)
		pod.SecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot: pointerTo(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		pod.NodeSelector = mt.Spec.Worker.NodeSelector
		pod.Tolerations = mt.Spec.Worker.Tolerations
		const scratch = "scanner-db-tmp"
		pod.Volumes = append(managerPodVolumes(mt), corev1.Volume{Name: scratch, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		pod.Containers = []corev1.Container{{
			Name: "acs-scanner-db", Image: os.Getenv("WORKER_IMAGE"), Args: args,
			Env: registryEnv(mt), VolumeMounts: append(managerContainerVolumeMounts(mt), corev1.VolumeMount{Name: scratch, MountPath: "/tmp"}),
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: storage}},
			SecurityContext: restrictedContainerSecurityContext(),
		}}
		return nil
	})
	return err
}
