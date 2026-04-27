package imagestate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

const testStateMirrored = "Mirrored"

func newFakeClient() *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme)
}

func TestImageEntry_Refs(t *testing.T) {
	entry := &ImageEntry{
		Source: "src",
		State:  "Pending",
	}

	if entry.HasImageSet("is1") {
		t.Fatal("unexpected ref")
	}

	entry.AddRef(ImageRef{ImageSet: "is1", Origin: OriginRelease})
	if !entry.HasImageSet("is1") {
		t.Fatal("missing ref")
	}

	entry.AddRef(ImageRef{ImageSet: "is1", Origin: OriginOperator, EntrySig: "sig"})
	if len(entry.Refs) != 1 || entry.Refs[0].Origin != OriginOperator {
		t.Fatal("ref update failed")
	}

	entry.AddRef(ImageRef{ImageSet: "is2"})
	if len(entry.Refs) != 2 {
		t.Fatal("adding second ref failed")
	}

	if !entry.RemoveImageSet("is1") {
		t.Fatal("remove failed")
	}
	if entry.HasImageSet("is1") {
		t.Fatal("ref still exists")
	}
	if len(entry.Refs) != 1 || entry.Refs[0].ImageSet != "is2" {
		t.Fatal("wrong refs remaining")
	}
}

func TestCountsForImageSet(t *testing.T) {
	state := ImageState{
		"img1": {State: testStateMirrored, Refs: []ImageRef{{ImageSet: "is1"}}},
		"img2": {State: testStateMirrored, Refs: []ImageRef{{ImageSet: "is2"}}},
		"img3": {State: "Pending", Refs: []ImageRef{{ImageSet: "is1"}, {ImageSet: "is2"}}},
		"img4": {State: "Failed", PermanentlyFailed: true, Refs: []ImageRef{{ImageSet: "is1"}}},
	}

	t.Run("is1", func(t *testing.T) {
		total, mirrored, pending, failed := CountsForImageSet(state, "is1")
		if total != 3 || mirrored != 1 || pending != 1 || failed != 1 {
			t.Fatalf("wrong counts for is1: %d %d %d %d", total, mirrored, pending, failed)
		}
	})

	t.Run("is2", func(t *testing.T) {
		total, mirrored, pending, failed := CountsForImageSet(state, "is2")
		if total != 2 || mirrored != 1 || pending != 1 || failed != 0 {
			t.Fatalf("wrong counts for is2: %d %d %d %d", total, mirrored, pending, failed)
		}
	})
}

func TestLoadSaveForTarget(t *testing.T) {
	c := newFakeClient().Build()
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt1", Namespace: "ns", UID: "uid-mt1"},
	}
	state := ImageState{"dest": {Source: "src", State: "Pending"}}

	if err := SaveForTarget(context.Background(), c, "ns", mt, state); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "mt1-images"}, cm); err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "MirrorTarget" {
		t.Fatal("wrong owner ref")
	}

	loaded, err := LoadForTarget(context.Background(), c, "ns", "mt1")
	if err != nil {
		t.Fatalf("LoadForTarget error: %v", err)
	}
	if len(loaded) != 1 || loaded["dest"].Source != "src" {
		t.Fatalf("unexpected loaded state: %v", loaded)
	}
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	original := ImageState{
		"img1": {
			Source: "src1", State: testStateMirrored,
			Refs: []ImageRef{{ImageSet: "is1", Origin: OriginRelease, EntrySig: "sig1"}},
		},
	}
	data, _ := encode(original)
	cm := &corev1.ConfigMap{BinaryData: map[string][]byte{"images.json.gz": data}}
	decoded, _ := decode(cm)
	if len(decoded) != 1 || decoded["img1"].Refs[0].EntrySig != "sig1" {
		t.Fatalf("roundtrip failed: %v", decoded["img1"])
	}
}

func TestCounts_Mixed(t *testing.T) {
	state := ImageState{
		"img1": {State: testStateMirrored},
		"img2": {State: "Pending"},
		"img3": {State: "Failed", PermanentlyFailed: true},
	}
	total, mirrored, pending, failed := Counts(state)
	if total != 3 || mirrored != 1 || pending != 1 || failed != 1 {
		t.Fatalf("counts failed: %d %d %d %d", total, mirrored, pending, failed)
	}
}

func TestConfigMapNames(t *testing.T) {
	if ConfigMapName("is") != "is-images" {
		t.Fail()
	}
	if ConfigMapNameForTarget("mt") != "mt-images" {
		t.Fail()
	}
}
