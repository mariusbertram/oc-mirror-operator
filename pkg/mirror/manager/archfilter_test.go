package manager

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func TestOwnerArchitectures(t *testing.T) {
	byIS := map[string][]string{
		"narrow": {"arm64", "amd64"},
		"other":  {"s390x", "amd64"},
		"all":    nil,
	}
	tests := []struct {
		owners []string
		want   []string
	}{
		{[]string{"narrow"}, []string{"amd64", "arm64"}},
		{[]string{"narrow", "other"}, []string{"amd64", "arm64", "s390x"}},
		{[]string{"narrow", "all"}, nil},
		{[]string{"unknown"}, nil},
		{[]string{"unknown", "narrow"}, []string{"amd64", "arm64"}},
		{nil, nil},
	}
	for _, tt := range tests {
		if got := ownerArchitectures(tt.owners, byIS); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ownerArchitectures(%v) = %v, want %v", tt.owners, got, tt.want)
		}
	}
}

func TestAnyOwnerEnabled(t *testing.T) {
	byIS := map[string]bool{"a": false, "b": true}
	if anyOwnerEnabled([]string{"a"}, byIS) || !anyOwnerEnabled([]string{"a", "b"}, byIS) || anyOwnerEnabled(nil, byIS) {
		t.Error("anyOwnerEnabled mismatch")
	}
}

// The ImageSet's architecture settings must reach the worker batch, per
// image and merged across owning ImageSets.
func TestDispatch_CarriesArchitectureFilter(t *testing.T) {
	m, cs := newReconcileTestManager(t, []string{"narrow", "plain"}, "narrow", "plain")
	ctx := context.Background()
	is := &mirrorv1alpha1.ImageSet{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: "narrow", Namespace: "default"}, is); err != nil {
		t.Fatal(err)
	}
	is.Spec.Mirror.Architectures = []string{"amd64", "arm64"}
	is.Spec.Mirror.SkipMissingArchitectures = true
	if err := m.Client.Update(ctx, is); err != nil {
		t.Fatal(err)
	}
	// Keep the ImageSet "resolved" so reconcile does not re-collect it.
	is.Status.ObservedGeneration = is.Generation
	if err := m.Client.Status().Update(ctx, is); err != nil {
		t.Fatal(err)
	}

	m.imageState = imagestate.ImageState{
		"reg.io/only-narrow:v1": {Source: "src/only-narrow:v1", State: statePending},
		"reg.io/shared:v1":      {Source: "src/shared:v1", State: statePending},
		"reg.io/only-plain:v1":  {Source: "src/only-plain:v1", State: statePending},
	}
	m.owners = map[string][]string{
		"reg.io/only-narrow:v1": {"narrow"},
		"reg.io/shared:v1":      {"narrow", "plain"},
		"reg.io/only-plain:v1":  {"plain"},
	}
	if err := m.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	pods, err := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]BatchItem{}
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				if e.Name != "MIRROR_BATCH" {
					continue
				}
				var items []BatchItem
				if err := json.Unmarshal([]byte(e.Value), &items); err != nil {
					t.Fatal(err)
				}
				for _, it := range items {
					got[it.Dest] = it
				}
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("dispatched %d items, want 3: %+v", len(got), got)
	}
	if it := got["reg.io/only-narrow:v1"]; !it.SkipMissingArchitectures || !reflect.DeepEqual(it.Architectures, []string{"amd64", "arm64"}) {
		t.Errorf("only-narrow = %+v", it)
	}
	// Shared with an ImageSet that wants every architecture: no list, but
	// skipping missing children is still allowed by "narrow".
	if it := got["reg.io/shared:v1"]; !it.SkipMissingArchitectures || it.Architectures != nil {
		t.Errorf("shared = %+v", it)
	}
	if it := got["reg.io/only-plain:v1"]; it.SkipMissingArchitectures || it.Architectures != nil {
		t.Errorf("only-plain = %+v", it)
	}
}
