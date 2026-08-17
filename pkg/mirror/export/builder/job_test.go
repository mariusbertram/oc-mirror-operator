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

package builder

import (
	"context"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func defaultMirrorExport() *mirrorv1alpha1.MirrorExport {
	return &mirrorv1alpha1.MirrorExport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: mirrorv1alpha1.GroupVersion.String(),
			Kind:       "MirrorExport",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-export",
			Namespace: "test-ns",
			UID:       "abc-123",
		},
		Spec: mirrorv1alpha1.MirrorExportSpec{
			Destination: mirrorv1alpha1.MirrorExportDestination{Registry: "registry.example.com/mirror"},
		},
	}
}

func TestNew_MissingEnvVar(t *testing.T) {
	t.Setenv(OperatorImageEnvVar, "")
	_, err := New()
	if err == nil {
		t.Fatal("expected an error when the operator image env var is unset")
	}
}

func TestNew_Success(t *testing.T) {
	t.Setenv(OperatorImageEnvVar, "registry.example.com/oc-mirror-operator:test")
	mgr, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.operatorImage != "registry.example.com/oc-mirror-operator:test" {
		t.Errorf("operatorImage = %q, want registry.example.com/oc-mirror-operator:test", mgr.operatorImage)
	}
}

func TestHostOnly(t *testing.T) {
	if got := hostOnly("registry.example.com/mirror"); got != "registry.example.com" {
		t.Errorf("hostOnly(with path) = %q, want registry.example.com", got)
	}
	if got := hostOnly("registry.example.com"); got != "registry.example.com" {
		t.Errorf("hostOnly(bare host) = %q, want registry.example.com", got)
	}
}

func TestSafeJobName_DNSCompliant(t *testing.T) {
	cases := [][]string{
		{"export-1"},
		{strings.Repeat("x", 200)},
		{"a-very-long-mirrorexport-name-that-keeps-going-and-going-and-going"},
	}
	for _, c := range cases {
		got := safeJobName("export-build", c...)
		if len(got) > 63 {
			t.Errorf("name %q too long (%d)", got, len(got))
		}
		if strings.ToLower(got) != got {
			t.Errorf("name %q not lower-case", got)
		}
		if strings.HasSuffix(got, "-") {
			t.Errorf("name %q ends with dash", got)
		}
	}
}

func TestJobName_Deterministic(t *testing.T) {
	a := JobName("my-export")
	b := JobName("my-export")
	if a != b {
		t.Errorf("JobName not deterministic: %q != %q", a, b)
	}
	if JobName("other-export") == a {
		t.Errorf("different export names produced the same Job name")
	}
}

func TestSignature_ChangesWithSpec(t *testing.T) {
	me := defaultMirrorExport()
	sig1 := Signature(me, `{"platform":{}}`)
	sig2 := Signature(me, `{"platform":{"graph":true}}`)
	if sig1 == sig2 {
		t.Errorf("expected different signatures for different mirror specs")
	}

	me2 := defaultMirrorExport()
	me2.Spec.Destination.Registry = "registry.other.example.com/mirror"
	sig3 := Signature(me2, `{"platform":{}}`)
	if sig1 == sig3 {
		t.Errorf("expected different signatures for different destination registries")
	}
}

func TestSignature_ChangesWithSource(t *testing.T) {
	me := defaultMirrorExport()
	sigNoSource := Signature(me, `{}`)

	me.Spec.Source = &mirrorv1alpha1.MirrorExportSource{
		Registry:   "registry.local.example.com/mirror",
		Insecure:   true,
		AuthSecret: "my-pull-secret",
	}
	sigWithSource := Signature(me, `{}`)
	if sigNoSource == sigWithSource {
		t.Errorf("expected the signature to change once Source is set")
	}
}

func TestEnsureExportJob_CreatesOnce(t *testing.T) {
	me := defaultMirrorExport()
	c := newFakeClient(me)
	mgr := &ExportBuildManager{operatorImage: "registry.example.com/oc-mirror-operator:test"}

	if err := mgr.EnsureExportJob(context.Background(), c, me, "test-export-export", "test-export-artifacts", `{}`); err != nil {
		t.Fatalf("EnsureExportJob() error = %v", err)
	}

	name := JobName(me.Name)
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: me.Namespace}, job); err != nil {
		t.Fatalf("expected Job %s to exist: %v", name, err)
	}
	if job.Spec.Template.Spec.ServiceAccountName != "test-export-export" {
		t.Errorf("ServiceAccountName = %q, want test-export-export", job.Spec.Template.Spec.ServiceAccountName)
	}

	// Calling again is a no-op: no error, no duplicate-create conflict.
	if err := mgr.EnsureExportJob(context.Background(), c, me, "test-export-export", "test-export-artifacts", `{}`); err != nil {
		t.Fatalf("EnsureExportJob() second call error = %v", err)
	}
}

func TestEnsureExportJob_GetErrorNonNotFound(t *testing.T) {
	me := defaultMirrorExport()
	c := newFakeClient(me)
	failing := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return fmt.Errorf("get failed")
		},
	})
	mgr := &ExportBuildManager{operatorImage: "registry.example.com/oc-mirror-operator:test"}

	err := mgr.EnsureExportJob(context.Background(), failing, me, "test-export-export", "test-export-artifacts", `{}`)
	if err == nil || !strings.Contains(err.Error(), "get failed") {
		t.Fatalf("expected the raw get error to propagate, got %v", err)
	}
}

func TestEnsureExportJob_CreateError(t *testing.T) {
	me := defaultMirrorExport()
	c := newFakeClient(me)
	failing := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			return fmt.Errorf("create failed")
		},
	})
	mgr := &ExportBuildManager{operatorImage: "registry.example.com/oc-mirror-operator:test"}

	err := mgr.EnsureExportJob(context.Background(), failing, me, "test-export-export", "test-export-artifacts", `{}`)
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("expected the raw create error to propagate, got %v", err)
	}
}

func TestGetExportJobStatus(t *testing.T) {
	me := defaultMirrorExport()
	name := JobName(me.Name)

	t.Run("not found", func(t *testing.T) {
		c := newFakeClient(me)
		phase, err := GetExportJobStatus(context.Background(), c, name, me.Namespace)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phase != JobPhaseNotFound {
			t.Errorf("phase = %q, want %q", phase, JobPhaseNotFound)
		}
	})

	t.Run("succeeded", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: me.Namespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}
		c := newFakeClient(me, job)
		phase, err := GetExportJobStatus(context.Background(), c, name, me.Namespace)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phase != JobPhaseSucceeded {
			t.Errorf("phase = %q, want %q", phase, JobPhaseSucceeded)
		}
	})

	t.Run("failed", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: me.Namespace},
			Status:     batchv1.JobStatus{Failed: 1},
		}
		c := newFakeClient(me, job)
		phase, err := GetExportJobStatus(context.Background(), c, name, me.Namespace)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phase != JobPhaseFailed {
			t.Errorf("phase = %q, want %q", phase, JobPhaseFailed)
		}
	})

	t.Run("running", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: me.Namespace},
			Status:     batchv1.JobStatus{Active: 1},
		}
		c := newFakeClient(me, job)
		phase, err := GetExportJobStatus(context.Background(), c, name, me.Namespace)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phase != JobPhaseRunning {
			t.Errorf("phase = %q, want %q", phase, JobPhaseRunning)
		}
	})

	t.Run("pending", func(t *testing.T) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: me.Namespace}}
		c := newFakeClient(me, job)
		phase, err := GetExportJobStatus(context.Background(), c, name, me.Namespace)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phase != JobPhasePending {
			t.Errorf("phase = %q, want %q", phase, JobPhasePending)
		}
	})

	t.Run("get error non-NotFound", func(t *testing.T) {
		c := newFakeClient(me)
		failing := interceptor.NewClient(c, interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return fmt.Errorf("get failed")
			},
		})
		_, err := GetExportJobStatus(context.Background(), failing, name, me.Namespace)
		if err == nil || !strings.Contains(err.Error(), "get failed") {
			t.Fatalf("expected the raw get error to propagate, got %v", err)
		}
	})
}

func TestDeleteExportJob(t *testing.T) {
	me := defaultMirrorExport()
	name := JobName(me.Name)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: me.Namespace}}
	c := newFakeClient(me, job)

	if err := DeleteExportJob(context.Background(), c, name, me.Namespace); err != nil {
		t.Fatalf("DeleteExportJob() error = %v", err)
	}
	if err := DeleteExportJob(context.Background(), c, name, me.Namespace); err != nil {
		t.Fatalf("DeleteExportJob() on already-deleted Job should be a no-op, got error = %v", err)
	}
}

func TestDeleteExportJob_GetErrorNonNotFound(t *testing.T) {
	me := defaultMirrorExport()
	name := JobName(me.Name)
	c := newFakeClient(me)
	failing := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return fmt.Errorf("get failed")
		},
	})
	err := DeleteExportJob(context.Background(), failing, name, me.Namespace)
	if err == nil || !strings.Contains(err.Error(), "get failed") {
		t.Fatalf("expected the raw get error to propagate, got %v", err)
	}
}

func TestBuildJobSpec_SourceAuthAndCABundle(t *testing.T) {
	me := defaultMirrorExport()
	me.Spec.Source = &mirrorv1alpha1.MirrorExportSource{
		Registry:   "registry.local.example.com/mirror",
		Insecure:   true,
		AuthSecret: "my-pull-secret",
		CABundle:   &mirrorv1alpha1.CABundleRef{ConfigMapName: "my-ca-bundle"},
	}
	mgr := &ExportBuildManager{operatorImage: "registry.example.com/oc-mirror-operator:test"}
	job := mgr.buildJobSpec(JobName(me.Name), me, "test-export-export", "test-export-artifacts", `{}`)

	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env[EnvDockerConfig] != "/var/run/secrets/registry" {
		t.Errorf("%s = %q, want /var/run/secrets/registry", EnvDockerConfig, env[EnvDockerConfig])
	}
	if env[EnvInsecureHosts] != "registry.local.example.com" {
		t.Errorf("%s = %q, want registry.local.example.com", EnvInsecureHosts, env[EnvInsecureHosts])
	}
	if env["SSL_CERT_FILE"] != "/run/secrets/ca/ca-bundle.crt" {
		t.Errorf("SSL_CERT_FILE = %q, want /run/secrets/ca/ca-bundle.crt", env["SSL_CERT_FILE"])
	}
}
