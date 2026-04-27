package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

const mirrorTargetFinalizer = "mirror.openshift.io/cleanup"

type MirrorTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *MirrorTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := r.Get(ctx, req.NamespacedName, mt); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !mt.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, mt)
	}
	if !controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
		controllerutil.AddFinalizer(mt, mirrorTargetFinalizer)
		if err := r.Update(ctx, mt); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.reconcileCleanup(ctx, mt); err != nil {
		l.Error(err, "Failed to reconcile cleanup")
	}
	if err := r.ensureCoordinatorRBAC(ctx, mt); err != nil {
		l.Error(err, "Failed to ensure coordinator RBAC")
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}
	_ = r.ensureNetworkPolicies(ctx, mt)
	_ = r.ensureResourceAPI(ctx, mt.Namespace)

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-manager", mt.Name), Namespace: mt.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if err := controllerutil.SetControllerReference(mt, deployment, r.Scheme); err != nil {
			return err
		}
		labels := map[string]string{"app": "oc-mirror-manager", "mirrortarget": mt.Name}
		deployment.Labels = labels
		replicas := int32(1)
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: mt.Name + "-coordinator",
					SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: pointerTo(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "manager", Image: os.Getenv("OPERATOR_IMAGE"),
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: pointerTo(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						Args:            []string{"manager", "--mirrortarget", mt.Name, "--namespace", mt.Namespace},
						Env:             managerContainerEnv(mt), VolumeMounts: managerContainerVolumeMounts(mt), Resources: mt.Spec.Manager.Resources,
						Ports: []corev1.ContainerPort{{Name: "status", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					}},
					Volumes: managerPodVolumes(mt), NodeSelector: mt.Spec.Manager.NodeSelector, Tolerations: mt.Spec.Manager.Tolerations,
				},
			},
		}
		return nil
	})
	if err != nil {
		setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error(), mt.Generation)
		_ = r.Status().Update(ctx, mt)
		return ctrl.Result{}, err
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-manager", mt.Name), Namespace: mt.Namespace}}
	_, _ = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		_ = controllerutil.SetControllerReference(mt, service, r.Scheme)
		service.Labels = map[string]string{"app": "oc-mirror-manager", "mirrortarget": mt.Name}
		service.Spec = corev1.ServiceSpec{Selector: service.Labels, Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}}
		return nil
	})
	_ = r.reconcileExposure(ctx, mt)
	setCondition(&mt.Status.Conditions, "Ready", metav1.ConditionTrue, "DeploymentReady", "Manager deployment is active", mt.Generation)
	if len(mt.Status.PendingCleanup) == 0 {
		mt.Status.KnownImageSets = make([]string, len(mt.Spec.ImageSets))
		copy(mt.Status.KnownImageSets, mt.Spec.ImageSets)
		sort.Strings(mt.Status.KnownImageSets)
	}
	if err := r.aggregateImageSetStatus(ctx, mt); err != nil {
		l.Error(err, "Failed to aggregate ImageSet status")
	}
	if err := r.Status().Update(ctx, mt); err != nil {
		l.Error(err, "Failed to update MirrorTarget status")
		return ctrl.Result{}, err
	}
	if len(mt.Status.PendingCleanup) > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *MirrorTargetReconciler) ensureResourceAPI(ctx context.Context, namespace string) error {
	name := "oc-mirror-resource-api"
	labels := map[string]string{"app": name}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		replicas := int32(1)
		dep.Labels = labels
		dep.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "oc-mirror-controller-manager",
					SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: pointerTo(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "api", Image: os.Getenv("OPERATOR_IMAGE"),
						Args:  []string{"resource-api", "--namespace", namespace},
						Ports: []corev1.ContainerPort{{ContainerPort: 8081, Name: "http"}},
						Env:   []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}},
					}},
				},
			},
		}
		return nil
	})
	if err != nil {
		return err
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec = corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8081), Name: "http"}}}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) aggregateImageSetStatus(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	state, err := imagestate.LoadForTarget(ctx, r.Client, mt.Namespace, mt.Name)
	if err != nil {
		return err
	}
	total, mirrored, pending, failed := imagestate.Counts(state)
	mt.Status.TotalImages, mt.Status.MirroredImages, mt.Status.PendingImages, mt.Status.FailedImages = total, mirrored, pending, failed
	names := make([]string, len(mt.Spec.ImageSets))
	copy(names, mt.Spec.ImageSets)
	sort.Strings(names)
	summaries := make([]mirrorv1alpha1.ImageSetStatusSummary, 0, len(names))
	for _, name := range names {
		isTotal, isMirrored, isPending, isFailed := imagestate.CountsForImageSet(state, name)
		summaries = append(summaries, mirrorv1alpha1.ImageSetStatusSummary{Name: name, Found: true, Total: isTotal, Mirrored: isMirrored, Pending: isPending, Failed: isFailed})
	}
	mt.Status.ImageSetStatuses = summaries
	return nil
}

func (r *MirrorTargetReconciler) reconcileCleanup(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	currentSet := make(map[string]bool, len(mt.Spec.ImageSets))
	for _, name := range mt.Spec.ImageSets {
		currentSet[name] = true
	}
	var removed []string
	for _, name := range mt.Status.KnownImageSets {
		if !currentSet[name] {
			removed = append(removed, name)
		}
	}
	stillPending := make([]string, 0, len(mt.Status.PendingCleanup))
	for _, name := range mt.Status.PendingCleanup {
		jobName := cleanupJobName(mt.Name, name)
		job := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: mt.Namespace}, job)
		if errors.IsNotFound(err) {
			snapshotName := cleanupSnapshotName(mt.Name, name)
			cm := &corev1.ConfigMap{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: mt.Namespace, Name: snapshotName}, cm); err == nil {
				stillPending = append(stillPending, name)
			}
			continue
		}
		if err == nil && job.Status.Succeeded > 0 {
			snapshotName := cleanupSnapshotName(mt.Name, name)
			_ = r.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: snapshotName, Namespace: mt.Namespace}})
			continue
		}
		if err == nil && job.Status.Failed > 0 {
			prop := metav1.DeletePropagationBackground
			_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		}
		stillPending = append(stillPending, name)
	}
	mt.Status.PendingCleanup = stillPending
	if len(removed) == 0 {
		if len(mt.Status.PendingCleanup) == 0 {
			for _, c := range mt.Status.Conditions {
				if c.Type == "Cleanup" && c.Reason == "CleanupInProgress" {
					setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionTrue, "CleanupComplete", "No pending cleanups", mt.Generation)
					break
				}
			}
		}
		return nil
	}
	if mt.Annotations[mirrorv1alpha1.CleanupPolicyAnnotation] != mirrorv1alpha1.CleanupPolicyDelete {
		return r.removeImageSetRefs(ctx, mt, removed)
	}
	for _, isName := range removed {
		alreadyPending := false
		for _, p := range mt.Status.PendingCleanup {
			if p == isName {
				alreadyPending = true
				break
			}
		}
		if alreadyPending {
			continue
		}
		state, err := imagestate.LoadForTarget(ctx, r.Client, mt.Namespace, mt.Name)
		if err != nil {
			continue
		}
		toDelete := make(imagestate.ImageState)
		for dest, entry := range state {
			if len(entry.Refs) == 1 && entry.Refs[0].ImageSet == isName {
				toDelete[dest] = entry
			}
		}
		if len(toDelete) == 0 {
			_ = r.removeImageSetRefs(ctx, mt, []string{isName})
			continue
		}
		snapshotName := cleanupSnapshotName(mt.Name, isName)
		if err := imagestate.SaveRaw(ctx, r.Client, mt.Namespace, snapshotName, toDelete); err != nil {
			continue
		}
		if err := r.createCleanupJob(ctx, mt, isName, snapshotName); err != nil {
			continue
		}
		_ = r.removeImageSetRefs(ctx, mt, []string{isName})
		mt.Status.PendingCleanup = append(mt.Status.PendingCleanup, isName)
	}
	if len(mt.Status.PendingCleanup) > 0 {
		setCondition(&mt.Status.Conditions, "Cleanup", metav1.ConditionFalse, "CleanupInProgress", "Cleaning up images", mt.Generation)
	}
	return nil
}

func (r *MirrorTargetReconciler) removeImageSetRefs(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, isNames []string) error {
	state, err := imagestate.LoadForTarget(ctx, r.Client, mt.Namespace, mt.Name)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range state {
		for _, isName := range isNames {
			if entry.RemoveImageSet(isName) {
				changed = true
			}
		}
	}
	if changed {
		return imagestate.SaveForTarget(ctx, r.Client, mt.Namespace, mt, state)
	}
	return nil
}

func (r *MirrorTargetReconciler) createCleanupJob(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSetName, snapshotName string) error {
	jobName := cleanupJobName(mt.Name, imageSetName)
	backoffLimit, ttl := int32(3), int32(600)
	labels := map[string]string{"app": "oc-mirror-operator", "mirrortarget": mt.Name, "mirror.openshift.io/cleanup": imageSetName}
	args := []string{"cleanup", "--snapshot", snapshotName, "--namespace", mt.Namespace, "--registry", mt.Spec.Registry}
	if mt.Spec.Insecure {
		args = append(args, "--insecure")
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	env := []corev1.EnvVar{}
	if mt.Spec.AuthSecret != "" {
		v := corev1.Volume{Name: "dockerconfig", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: mt.Spec.AuthSecret, Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}}}}}
		volumes = append(volumes, v)
		mounts = append(mounts, corev1.VolumeMount{Name: "dockerconfig", MountPath: "/docker-config", ReadOnly: true})
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/docker-config"})
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: mt.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(mt, mirrorv1alpha1.GroupVersion.WithKind("MirrorTarget"))}},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: mt.Name + "-coordinator", RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: pointerTo(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers:      []corev1.Container{{Name: "cleanup", Image: os.Getenv("OPERATOR_IMAGE"), Args: args, Env: env, VolumeMounts: mounts, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: pointerTo(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
					Volumes:         volumes, NodeSelector: mt.Spec.Worker.NodeSelector, Tolerations: mt.Spec.Worker.Tolerations,
				},
			},
		},
	}
	return r.Create(ctx, job)
}

func cleanupSnapshotName(targetName, imageSetName string) string {
	h := sha256.New()
	h.Write([]byte(targetName))
	h.Write([]byte{1})
	h.Write([]byte(imageSetName))
	return fmt.Sprintf("snapshot-%s-%s-%x", targetName, imageSetName, h.Sum(nil))[:63]
}

func cleanupJobName(targetName, imageSetName string) string {
	h := sha256.New()
	h.Write([]byte(targetName))
	h.Write([]byte{0})
	h.Write([]byte(imageSetName))
	return fmt.Sprintf("cleanup-%s-%s-%x", targetName, imageSetName, h.Sum(nil))[:63]
}

func (r *MirrorTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&mirrorv1alpha1.MirrorTarget{}).Watches(&mirrorv1alpha1.ImageSet{}, handler.EnqueueRequestsFromMapFunc(r.mirrorTargetsForImageSet)).Complete(r)
}

func (r *MirrorTargetReconciler) mirrorTargetsForImageSet(ctx context.Context, obj client.Object) []reconcile.Request {
	is, _ := obj.(*mirrorv1alpha1.ImageSet)
	var mtList mirrorv1alpha1.MirrorTargetList
	if err := r.List(ctx, &mtList, client.InNamespace(is.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, mt := range mtList.Items {
		for _, name := range mt.Spec.ImageSets {
			if name == is.Name {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: mt.Namespace, Name: mt.Name}})
				break
			}
		}
	}
	return reqs
}

func (r *MirrorTargetReconciler) ensureCoordinatorRBAC(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	saName := mt.Name + "-coordinator"
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: mt.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error { return controllerutil.SetControllerReference(mt, sa, r.Scheme) }); err != nil {
		return err
	}
	roleName := mt.Name + "-coordinator"
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: mt.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "secrets", "configmaps"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"mirror.openshift.io"}, Resources: []string{"mirrortargets", "imagesets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"mirror.openshift.io"}, Resources: []string{"mirrortargets/status", "imagesets/status"}, Verbs: []string{"get", "update", "patch"}},
		}
		return controllerutil.SetControllerReference(mt, role, r.Scheme)
	}); err != nil {
		return err
	}
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: mt.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.RoleRef = rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: roleName}
		rb.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: mt.Namespace}}
		return controllerutil.SetControllerReference(mt, rb, r.Scheme)
	}); err != nil {
		return err
	}
	return nil
}

func (r *MirrorTargetReconciler) ensureNetworkPolicies(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	name := mt.Name + "-allow-manager"
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mt.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec = networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "oc-mirror-manager", "mirrortarget": mt.Name}}, Ingress: []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Port: pointerTo(intstr.FromInt(8080))}, {Port: pointerTo(intstr.FromInt(8081))}}}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}
		return controllerutil.SetControllerReference(mt, np, r.Scheme)
	}); err != nil {
		return err
	}
	return nil
}

func (r *MirrorTargetReconciler) reconcileExposure(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) error {
	exposeType := mirrorv1alpha1.ExposeTypeService
	if mt.Spec.Expose != nil && mt.Spec.Expose.Type != "" {
		exposeType = mt.Spec.Expose.Type
	} else if r.hasRouteAPI(ctx) {
		exposeType = mirrorv1alpha1.ExposeTypeRoute
	}
	svcName := "oc-mirror-resource-api"
	if exposeType != mirrorv1alpha1.ExposeTypeRoute {
		r.deleteRoute(ctx, mt)
	}
	if exposeType != mirrorv1alpha1.ExposeTypeIngress {
		r.deleteIngress(ctx, mt)
	}
	switch exposeType {
	case mirrorv1alpha1.ExposeTypeRoute:
		return r.ensureRoute(ctx, mt, svcName)
	case mirrorv1alpha1.ExposeTypeIngress:
		return r.ensureIngress(ctx, mt, svcName)
	default:
		return nil
	}
}

func (r *MirrorTargetReconciler) hasRouteAPI(ctx context.Context) bool {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "__probe__"}, route)
	if err == nil {
		return true
	}
	if meta.IsNoMatchError(err) {
		return false
	}
	return errors.IsNotFound(err)
}

func (r *MirrorTargetReconciler) ensureRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	route.SetName(fmt.Sprintf("%s-resources", mt.Name))
	route.SetNamespace(mt.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		if err := controllerutil.SetControllerReference(mt, route, r.Scheme); err != nil {
			return err
		}
		spec := map[string]interface{}{"to": map[string]interface{}{"kind": "Service", "name": svcName}, "port": map[string]interface{}{"targetPort": "http"}, "tls": map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"}}
		if mt.Spec.Expose != nil && mt.Spec.Expose.Host != "" {
			spec["host"] = mt.Spec.Expose.Host
		}
		route.Object["spec"] = spec
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) ensureIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, svcName string) error {
	if mt.Spec.Expose == nil || mt.Spec.Expose.Host == "" {
		return fmt.Errorf("ingress requires host")
	}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-resources", mt.Name), Namespace: mt.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		if err := controllerutil.SetControllerReference(mt, ingress, r.Scheme); err != nil {
			return err
		}
		pathType := networkingv1.PathTypePrefix
		ingress.Spec = networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: mt.Spec.Expose.Host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/api", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svcName, Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}}}
		if mt.Spec.Expose.IngressClassName != "" {
			ingress.Spec.IngressClassName = &mt.Spec.Expose.IngressClassName
		}
		return nil
	})
	return err
}

func (r *MirrorTargetReconciler) deleteRoute(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	route.SetName(fmt.Sprintf("%s-resources", mt.Name))
	route.SetNamespace(mt.Namespace)
	_ = r.Delete(ctx, route)
}

func (r *MirrorTargetReconciler) deleteIngress(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) {
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-resources", mt.Name), Namespace: mt.Namespace}}
	_ = r.Delete(ctx, ingress)
}

func (r *MirrorTargetReconciler) handleDeletion(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mt, mirrorTargetFinalizer) {
		return ctrl.Result{}, nil
	}
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: mt.Name + "-manager", Namespace: mt.Namespace}, dep); err == nil {
		_ = r.Delete(ctx, dep)
	}
	podList := &corev1.PodList{}
	_ = r.List(ctx, podList, client.InNamespace(mt.Namespace), client.MatchingLabels{"mirrortarget": mt.Name})
	for i := range podList.Items {
		_ = r.Delete(ctx, &podList.Items[i])
	}
	if len(podList.Items) > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(mt, mirrorTargetFinalizer)
	return ctrl.Result{}, r.Update(ctx, mt)
}

func managerContainerEnv(mt *mirrorv1alpha1.MirrorTarget) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}, {Name: "OPERATOR_IMAGE", Value: os.Getenv("OPERATOR_IMAGE")}}
	if mt.Spec.AuthSecret != "" {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/docker-config"})
	}
	env = append(env, caBundleEnvVars(mt.Spec.CABundle)...)
	env = append(env, workerProxyEnvVars(mt.Spec.Proxy)...)
	return env
}

func caBundleEnvVars(cfg *mirrorv1alpha1.CABundleRef) []corev1.EnvVar {
	if cfg == nil {
		return nil
	}
	key := cfg.Key
	if key == "" {
		key = "ca-bundle.crt"
	}
	return []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: "/run/secrets/ca/" + key}}
}

func workerProxyEnvVars(cfg *mirrorv1alpha1.ProxyConfig) []corev1.EnvVar {
	if cfg == nil {
		return nil
	}
	var env []corev1.EnvVar
	if v := cfg.HTTPProxy; v != "" {
		env = append(env, corev1.EnvVar{Name: "HTTP_PROXY", Value: v}, corev1.EnvVar{Name: "http_proxy", Value: v})
	}
	if v := cfg.HTTPSProxy; v != "" {
		env = append(env, corev1.EnvVar{Name: "HTTPS_PROXY", Value: v}, corev1.EnvVar{Name: "https_proxy", Value: v})
	}
	if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
		noProxy := workerBuildEffectiveNoProxy(cfg.NoProxy)
		env = append(env, corev1.EnvVar{Name: "NO_PROXY", Value: noProxy}, corev1.EnvVar{Name: "no_proxy", Value: noProxy}, corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: "kubernetes.default.svc.cluster.local"})
	} else if v := cfg.NoProxy; v != "" {
		env = append(env, corev1.EnvVar{Name: "NO_PROXY", Value: v}, corev1.EnvVar{Name: "no_proxy", Value: v})
	}
	return env
}

func workerBuildEffectiveNoProxy(userNoProxy string) string {
	base := strings.Join([]string{"localhost", "127.0.0.1", ".svc", ".svc.cluster.local"}, ",")
	if userNoProxy == "" {
		return base
	}
	return base + "," + userNoProxy
}

func managerContainerVolumeMounts(mt *mirrorv1alpha1.MirrorTarget) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{}
	if mt.Spec.AuthSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "dockerconfig", MountPath: "/docker-config", ReadOnly: true})
	}
	if mt.Spec.CABundle != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: "ca-bundle", MountPath: "/run/secrets/ca", ReadOnly: true})
	}
	return mounts
}

func managerPodVolumes(mt *mirrorv1alpha1.MirrorTarget) []corev1.Volume {
	vols := []corev1.Volume{}
	if mt.Spec.AuthSecret != "" {
		v := corev1.Volume{Name: "dockerconfig", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: mt.Spec.AuthSecret, Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}}}}}
		vols = append(vols, v)
	}
	if mt.Spec.CABundle != nil {
		key := mt.Spec.CABundle.Key
		if key == "" {
			key = "ca-bundle.crt"
		}
		v := corev1.Volume{Name: "ca-bundle", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: mt.Spec.CABundle.ConfigMapName}, Items: []corev1.KeyToPath{{Key: key, Path: key}}}}}
		vols = append(vols, v)
	}
	return vols
}

func pointerTo[T any](v T) *T { return &v }
