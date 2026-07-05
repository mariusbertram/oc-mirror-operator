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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

func newFakeClient(objs ...client.Object) client.Client {
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
