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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	ocmetrics "github.com/mariusbertram/oc-mirror-operator/pkg/metrics"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/export"
	exportbuilder "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/export/builder"
)

// MirrorExportReconciler reconciles a MirrorExport object.
//
// Unlike MirrorTargetReconciler/ImageSetReconciler, this reconciler never
// drives continuous mirroring: it resolves content once per spec generation
// (via a one-shot Job, since resolution needs registry credentials the
// controller-manager process itself does not hold) and publishes the result
// as a downloadable ConfigMap. See https://github.com/mariusbertram/oc-mirror-operator/issues/82
// for the full design and its explicit scope: building the operator-catalog
// overlay and graph-data image, and the actual bulk image copy on either
// side of an air gap, are deliberately out of scope for this reconciler.
type MirrorExportReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	ExportBuildMgr *exportbuilder.ExportBuildManager
}

// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrorexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrorexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mirror.openshift.io,resources=mirrorexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch

// ArtifactsConfigMapName returns the deterministic name of the ConfigMap a
// MirrorExport's rendered artifacts are published to.
func ArtifactsConfigMapName(exportName string) string {
	return exportName + "-artifacts"
}

func exportServiceAccountName(exportName string) string {
	return exportName + "-export"
}

func (r *MirrorExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, rerr error) {
	l := log.FromContext(ctx)

	defer func() {
		if rerr != nil {
			ocmetrics.ReconcileErrorsTotal.WithLabelValues(req.Namespace, req.Name, "mirrorexport").Inc()
		}
	}()

	me := &mirrorv1alpha1.MirrorExport{}
	if err := r.Get(ctx, req.NamespacedName, me); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	mirrorSpecJSON, err := json.Marshal(me.Spec.Mirror)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal Mirror spec: %w", err)
	}

	cmName := ArtifactsConfigMapName(me.Name)
	saName := exportServiceAccountName(me.Name)

	if err := r.ensureExportRBAC(ctx, me, saName, cmName); err != nil {
		l.Error(err, "failed to ensure export RBAC")
		setCondition(&me.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "RBACFailed", err.Error(), me.Generation)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, me)
	}

	if err := r.ensureArtifactsConfigMap(ctx, me, cmName); err != nil {
		l.Error(err, "failed to ensure artifacts ConfigMap")
		setCondition(&me.Status.Conditions, conditionTypeReady, metav1.ConditionFalse, "ConfigMapFailed", err.Error(), me.Generation)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, me)
	}

	sig := exportbuilder.Signature(me, string(mirrorSpecJSON))
	jobName := exportbuilder.JobName(me.Name)
	phase, err := exportbuilder.GetExportJobStatus(ctx, r.Client, jobName, me.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Recreate the Job when the spec changed since the last render.
	if me.Status.LastRenderedSignature != "" && me.Status.LastRenderedSignature != sig && phase != exportbuilder.JobPhaseNotFound {
		l.Info("MirrorExport spec changed, recreating export build Job", "job", jobName)
		if delErr := exportbuilder.DeleteExportJob(ctx, r.Client, jobName, me.Namespace); delErr != nil {
			l.Error(delErr, "failed to delete stale export build Job", "job", jobName)
		}
		phase = exportbuilder.JobPhaseNotFound
	}

	// Job was TTL-cleaned but this exact spec was already rendered successfully: nothing to do.
	if phase == exportbuilder.JobPhaseNotFound && me.Status.LastRenderedSignature == sig && me.Status.ArtifactsConfigMap != "" {
		return ctrl.Result{}, nil
	}

	if phase == exportbuilder.JobPhaseNotFound {
		if err := r.ExportBuildMgr.EnsureExportJob(ctx, r.Client, me, saName, cmName, string(mirrorSpecJSON)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to ensure export build Job: %w", err)
		}
		phase, err = exportbuilder.GetExportJobStatus(ctx, r.Client, jobName, me.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	l.Info("export build Job status", "job", jobName, "phase", phase)

	switch phase {
	case exportbuilder.JobPhaseSucceeded:
		return ctrl.Result{}, r.recordSuccess(ctx, me, cmName, sig)
	case exportbuilder.JobPhaseFailed:
		setCondition(&me.Status.Conditions, conditionTypeReady, metav1.ConditionFalse,
			"ExportBuildFailed", "export build Job failed; see Job/Pod logs for details", me.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, me)
	default:
		setCondition(&me.Status.Conditions, conditionTypeReady, metav1.ConditionFalse,
			"ExportBuildRunning", "export build Job is resolving content", me.Generation)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.Status().Update(ctx, me)
	}
}

// recordSuccess reads the rendered manifest back from the artifacts
// ConfigMap to report TotalImages, and marks the MirrorExport Ready.
func (r *MirrorExportReconciler) recordSuccess(ctx context.Context, me *mirrorv1alpha1.MirrorExport, cmName, sig string) error {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: me.Namespace}, cm); err != nil {
		return fmt.Errorf("failed to read artifacts ConfigMap %s: %w", cmName, err)
	}

	total := 0
	if raw, ok := cm.Data["manifest.json"]; ok {
		var manifest export.Manifest
		if err := json.Unmarshal([]byte(raw), &manifest); err == nil {
			total = len(manifest.Images)
		}
	}

	me.Status.ArtifactsConfigMap = cmName
	me.Status.LastRenderedSignature = sig
	me.Status.TotalImages = total
	setCondition(&me.Status.Conditions, conditionTypeReady, metav1.ConditionTrue,
		"Rendered", "content resolved and artifacts published", me.Generation)
	return r.Status().Update(ctx, me)
}

// ensureArtifactsConfigMap creates the empty artifacts ConfigMap the export
// build Job writes into. Data is deliberately never touched here after
// creation — only the Job (via a Get+Update, see cmd/export-builder) writes
// Data, so a reconcile that runs while a render is in flight or already
// complete never clobbers it.
func (r *MirrorExportReconciler) ensureArtifactsConfigMap(ctx context.Context, me *mirrorv1alpha1.MirrorExport, name string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: me.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels["app.kubernetes.io/managed-by"] = "oc-mirror-operator"
		cm.Labels["mirror.openshift.io/export"] = me.Name
		return controllerutil.SetControllerReference(me, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to create artifacts ConfigMap: %w", err)
	}
	return nil
}

// ensureExportRBAC creates the ServiceAccount, Role, and RoleBinding the
// export build Job runs as. The Role is scoped via resourceNames to the
// single artifacts ConfigMap this MirrorExport owns — the Job never needs
// broader ConfigMap access than that.
func (r *MirrorExportReconciler) ensureExportRBAC(ctx context.Context, me *mirrorv1alpha1.MirrorExport, saName, cmName string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: me.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(me, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to create export ServiceAccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: me.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		if err := controllerutil.SetControllerReference(me, role, r.Scheme); err != nil {
			return err
		}
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"configmaps"},
				ResourceNames: []string{cmName},
				Verbs:         []string{"get", "update", "patch"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create export Role: %w", err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: me.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		if err := controllerutil.SetControllerReference(me, rb, r.Scheme); err != nil {
			return err
		}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     saName,
		}
		rb.Subjects = []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: saName, Namespace: me.Namespace},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create export RoleBinding: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MirrorExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bm, err := exportbuilder.New()
	if err != nil {
		return fmt.Errorf("init export build manager: %w", err)
	}
	r.ExportBuildMgr = bm

	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.MirrorExport{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Complete(r)
}
