package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("registering twice: %v", err)
	}
	for _, kind := range []string{
		"ImageSet", "ImageSetList",
		"MirrorTarget", "MirrorTargetList",
		"MirrorExport", "MirrorExportList",
	} {
		t.Run(kind, func(t *testing.T) {
			gvk := GroupVersion.WithKind(kind)
			obj, err := scheme.New(gvk)
			if err != nil {
				t.Fatal(err)
			}
			kinds, unversioned, err := scheme.ObjectKinds(obj)
			if err != nil {
				t.Fatal(err)
			}
			if unversioned || len(kinds) != 1 || kinds[0] != gvk {
				t.Fatalf("unexpected registered kinds: %v, unversioned=%v", kinds, unversioned)
			}
		})
	}
	if _, err := scheme.New(GroupVersion.WithKind("WatchEvent")); err != nil {
		t.Fatalf("metadata types not registered: %v", err)
	}
	if kinds, _, err := scheme.ObjectKinds(&metav1.ListOptions{}); err != nil || len(kinds) == 0 {
		t.Fatalf("list options not registered: %v", err)
	}
}
