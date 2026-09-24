package manager

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// CatalogSources are generated from the explicit Catalog field, not from the
// display label (#146) — with a fallback for entries written before it existed.
func TestSaveGlobalResources_UsesCatalogField(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = mirrorv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewWithClients(c, nil, "test", "default", "img", "", scheme)
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io"},
	}
	m.imageState = imagestate.ImageState{
		// Label in some future, differently formatted form: only Catalog counts.
		"reg.io/a:sha256-1": {Origin: imagestate.OriginOperator, Catalog: "quay.io/new/catalog:v1", OriginRef: "Operators from quay.io/new/catalog:v1"},
		// Written before Catalog existed: falls back to the label.
		"reg.io/b:sha256-2": {Origin: imagestate.OriginOperator, OriginRef: "quay.io/old/catalog:v2 [pkg]"},
		// Not an operator entry.
		"reg.io/c:v1": {Origin: imagestate.OriginAdditional, OriginRef: "additional"},
	}

	if err := m.saveGlobalResources(context.Background(), mt, &mirrorv1alpha1.ImageSetList{}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "oc-mirror-test-resources"}, cm); err != nil {
		t.Fatal(err)
	}
	var sources []string
	for key, val := range cm.Data {
		if strings.HasPrefix(key, "catalogsource-") {
			sources = append(sources, val)
		}
	}
	if len(sources) != 2 {
		t.Fatalf("got %d CatalogSources, want 2: %v", len(sources), cm.Data)
	}
	all := strings.Join(sources, "\n")
	for _, want := range []string{"new", "old"} {
		if !strings.Contains(all, want) {
			t.Errorf("no CatalogSource for the %q catalog in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "Operators") {
		t.Errorf("the display label must not be parsed as a catalog:\n%s", all)
	}
}

func TestCarryOver_BackfillsCatalog(t *testing.T) {
	src := imagestate.ImageState{
		"d": {Origin: imagestate.OriginOperator, EntrySig: "s", OriginRef: "quay.io/cat:v1 [pkg]"},
	}
	dst := imagestate.ImageState{}
	carryOverByOriginAndSig(src, dst, imagestate.OriginOperator, "s", "quay.io/cat:v1 [pkg]")
	if got := dst["d"].Catalog; got != "quay.io/cat:v1" {
		t.Errorf("Catalog = %q, want back-filled quay.io/cat:v1", got)
	}
}

func TestCatalogFromOriginRef(t *testing.T) {
	for ref, want := range map[string]string{
		"quay.io/cat:v1 [a, b]":              "quay.io/cat:v1",
		"quay.io/cat:v1 [a] — bundle.v1.2.3": "quay.io/cat:v1",
		"quay.io/cat:v1":                     "quay.io/cat:v1",
		"":                                   "",
	} {
		if got := imagestate.CatalogFromOriginRef(ref); got != want {
			t.Errorf("CatalogFromOriginRef(%q) = %q, want %q", ref, got, want)
		}
	}
}
