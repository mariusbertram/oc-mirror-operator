package cleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

const (
	ns = "ns"
	cm = "is-images"
)

type fakeDeleter struct {
	deleted *[]string
	fail    map[string]bool
}

func (d fakeDeleter) DeleteManifest(_ context.Context, image string) error {
	if d.fail[image] {
		return errors.New("denied")
	}
	*d.deleted = append(*d.deleted, image)
	return nil
}

func newClient(t *testing.T, state imagestate.ImageState) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if state != nil {
		if err := imagestate.SaveRaw(context.Background(), c, ns, cm, state, nil, nil); err != nil {
			t.Fatalf("SaveRaw: %v", err)
		}
	}
	return c
}

func cmExists(t *testing.T, c client.Client) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: cm}, &corev1.ConfigMap{})
	if err != nil && !k8serrors.IsNotFound(err) {
		t.Fatalf("Get: %v", err)
	}
	return err == nil
}

func TestRun(t *testing.T) {
	mixed := imagestate.ImageState{
		"reg/a": {State: "Mirrored"},
		"reg/b": {State: "Mirrored"},
		"reg/c": {State: "Pending"},
	}
	tests := []struct {
		name        string
		state       imagestate.ImageState
		fail        map[string]bool
		want        Result
		wantErr     bool
		wantCMAfter bool
	}{
		{name: "deletes mirrored images and the ConfigMap", state: mixed, want: Result{Deleted: 2, Skipped: 1}},
		{name: "keeps the ConfigMap when a deletion fails", state: mixed, fail: map[string]bool{"reg/b": true},
			want: Result{Deleted: 1, Skipped: 1, Failed: 1}, wantErr: true, wantCMAfter: true},
		{name: "empty state only removes the ConfigMap", state: imagestate.ImageState{}},
		{name: "missing ConfigMap is a no-op"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.state)
			var deleted []string
			got, err := Run(context.Background(), c, ns, cm, func() Deleter { return fakeDeleter{deleted: &deleted, fail: tt.fail} })
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
			if cmExists(t, c) != tt.wantCMAfter {
				t.Fatalf("ConfigMap exists = %v, want %v", !tt.wantCMAfter, tt.wantCMAfter)
			}
		})
	}
}

func TestRun_RefreshesDeleterEveryInterval(t *testing.T) {
	state := imagestate.ImageState{}
	for i := 0; i < 2*ClientRefreshInterval+1; i++ {
		state[fmt.Sprintf("reg/%d", i)] = &imagestate.ImageEntry{State: "Mirrored"}
	}
	c := newClient(t, state)
	var deleted []string
	built := 0
	res, err := Run(context.Background(), c, ns, cm, func() Deleter {
		built++
		return fakeDeleter{deleted: &deleted}
	})
	if err != nil || res.Deleted != len(state) {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if built != 3 {
		t.Fatalf("built %d deleters, want 3", built)
	}
}

func TestRun_LoadError(t *testing.T) {
	// Scheme without corev1: loading the ConfigMap fails.
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if _, err := Run(context.Background(), c, ns, cm, nil); err == nil {
		t.Fatal("expected load error")
	}
}

func TestRun_ConfigMapDeleteErrorIsOnlyLogged(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	if _, err := Run(context.Background(), c, ns, cm, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		ok     bool
		wantCM string
	}{
		{"derives ConfigMap from ImageSet", []string{"--namespace=ns", "--registry=reg", "--imageset=is"}, true, "is-images"},
		{"explicit ConfigMap", []string{"--namespace=ns", "--registry=reg", "--configmap=orph", "--insecure"}, true, "orph"},
		{"missing registry", []string{"--namespace=ns", "--imageset=is"}, false, ""},
		{"missing imageset and configmap", []string{"--namespace=ns", "--registry=reg"}, false, ""},
		{"unknown flag", []string{"--nope"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, ok := ParseFlags(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && o.ConfigMap != tt.wantCM {
				t.Fatalf("ConfigMap = %q, want %q", o.ConfigMap, tt.wantCM)
			}
		})
	}
}

func TestMainAndRunWrapper(t *testing.T) {
	if got := Main([]string{"--nope"}, runtime.NewScheme()); got != 1 {
		t.Fatalf("Main with bad flags = %d", got)
	}
	opts := Options{Namespace: ns, Registry: "reg.example", ConfigMap: cm}
	// Empty state: succeeds without contacting the registry.
	if got := run(context.Background(), newClient(t, imagestate.ImageState{}), opts); got != 0 {
		t.Fatalf("run = %d", got)
	}
	bad := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if got := run(context.Background(), bad, opts); got != 1 {
		t.Fatalf("run with load error = %d", got)
	}
}

func TestMain_KubeConfig(t *testing.T) {
	args := []string{"--namespace=ns", "--registry=reg", "--imageset=is"}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", t.TempDir())
	if got := Main(args, runtime.NewScheme()); got != 1 {
		t.Fatalf("Main without config = %d", got)
	}

	// A reachable config whose API server refuses connections: the client
	// is built, loading the state fails.
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "http://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
users: [{name: u, user: {}}]
current-context: c
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if got := Main(args, scheme); got != 1 {
		t.Fatalf("Main against unreachable API server = %d", got)
	}
}
