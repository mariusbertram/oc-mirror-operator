package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// --- helpers ---

const testStateMirrored = "Mirrored"

func mustGzip(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

func newFakeClient() *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme)
}

// --- ConfigMapName ---

func TestConfigMapName(t *testing.T) {
	if got := ConfigMapName("my-imageset"); got != "my-imageset-images" {
		t.Fatalf("expected my-imageset-images, got %s", got)
	}
	if got := ConfigMapName(""); got != "-images" {
		t.Fatalf("expected -images, got %s", got)
	}
}

// --- Counts ---

func TestCounts_Empty(t *testing.T) {
	total, mirrored, pending, failed := Counts(ImageState{})
	if total != 0 || mirrored != 0 || pending != 0 || failed != 0 {
		t.Fatalf("expected all zeros, got %d %d %d %d", total, mirrored, pending, failed)
	}
}

func TestCounts_Mixed(t *testing.T) {
	state := ImageState{
		"img1": {State: testStateMirrored},
		"img2": {State: testStateMirrored},
		"img3": {State: "Pending"},
		"img4": {State: "Failed", PermanentlyFailed: true},
		"img5": {State: "Pending", PermanentlyFailed: true},
		"img6": {State: "Pending"},
	}
	total, mirrored, pending, failed := Counts(state)
	if total != 6 {
		t.Fatalf("expected total=6, got %d", total)
	}
	if mirrored != 2 {
		t.Fatalf("expected mirrored=2, got %d", mirrored)
	}
	if pending != 2 {
		t.Fatalf("expected pending=2, got %d", pending)
	}
	if failed != 2 {
		t.Fatalf("expected failed=2, got %d", failed)
	}
}

func TestCounts_MirroredNotCountedAsFailed(t *testing.T) {
	// Even if PermanentlyFailed is set, Mirrored state takes precedence
	state := ImageState{
		"img1": {State: testStateMirrored, PermanentlyFailed: true},
	}
	total, mirrored, _, failed := Counts(state)
	if total != 1 || mirrored != 1 || failed != 0 {
		t.Fatalf("Mirrored should take precedence over PermanentlyFailed")
	}
}

// --- encode / decode roundtrip ---

func TestEncodeDecode_Roundtrip(t *testing.T) {
	original := ImageState{
		"registry.example.com/repo@sha256:abc": {
			Source:            "quay.io/source@sha256:abc",
			State:             testStateMirrored,
			Origin:            OriginRelease,
			EntrySig:          "sig123",
			OriginRef:         "stable-4.14 [amd64]",
			PermanentlyFailed: false,
		},
		"registry.example.com/repo2:v1.0": {
			Source:            "docker.io/library/alpine:3.18",
			State:             "Pending",
			RetryCount:        3,
			LastError:         "timeout",
			Origin:            OriginAdditional,
			PermanentlyFailed: false,
		},
	}

	data, err := encode(original)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encoded data is empty")
	}

	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{"images.json.gz": data},
	}
	decoded, err := decode(cm)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(decoded) != len(original) {
		t.Fatalf("decoded length %d != original %d", len(decoded), len(original))
	}
	for k, orig := range original {
		got, ok := decoded[k]
		if !ok {
			t.Fatalf("missing key %s in decoded state", k)
		}
		if got.Source != orig.Source || got.State != orig.State || got.RetryCount != orig.RetryCount ||
			got.LastError != orig.LastError || got.Origin != orig.Origin || got.EntrySig != orig.EntrySig {
			t.Fatalf("decoded entry mismatch for %s", k)
		}
	}
}

func TestEncode_EmptyState(t *testing.T) {
	data, err := encode(ImageState{})
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{"images.json.gz": data},
	}
	decoded, err := decode(cm)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected empty decoded state, got %d entries", len(decoded))
	}
}

func TestDecode_PlainJSON(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"images.json": `{"img1":{"source":"src1","state":"Mirrored"}}`,
		},
	}
	state, err := decode(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(state))
	}
	if state["img1"].State != testStateMirrored {
		t.Fatalf("expected Mirrored, got %s", state["img1"].State)
	}
}

func TestDecode_CorruptGzipReturnsError(t *testing.T) {
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{
			"images.json.gz": []byte("not-actually-gzip-data"),
		},
	}
	state, err := decode(cm)
	if err == nil {
		t.Fatalf("expected error decoding corrupt gzip, got state=%v", state)
	}
	if state != nil {
		t.Fatalf("expected nil state on error, got %v", state)
	}
}

func TestDecode_CorruptJSONReturnsError(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			"images.json": "{not-valid-json",
		},
	}
	state, err := decode(cm)
	if err == nil {
		t.Fatalf("expected error decoding corrupt json, got state=%v", state)
	}
	if state != nil {
		t.Fatalf("expected nil state on error, got %v", state)
	}
}

func TestDecode_EmptyConfigMapReturnsEmptyState(t *testing.T) {
	cm := &corev1.ConfigMap{}
	state, err := decode(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
}

func TestDecode_CorruptGzippedJSON(t *testing.T) {
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{"images.json.gz": mustGzip([]byte("{broken"))},
	}
	_, err := decode(cm)
	if err == nil {
		t.Fatal("expected error for valid gzip but corrupt JSON")
	}
}

// --- ptr ---

func TestPtr(t *testing.T) {
	v := ptr(true)
	if v == nil || *v != true {
		t.Fatal("ptr(true) failed")
	}
	s := ptr("hello")
	if s == nil || *s != "hello" {
		t.Fatal("ptr(string) failed")
	}
}

// --- Load / LoadByConfigMapName (with fake client) ---

func TestLoad_MissingConfigMap(t *testing.T) {
	c := newFakeClient().Build()
	state, err := Load(context.Background(), c, "ns", "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || len(state) != 0 {
		t.Fatalf("expected empty non-nil state, got %v", state)
	}
}

func TestLoad_ExistingConfigMap(t *testing.T) {
	original := ImageState{"dest": {Source: "src", State: testStateMirrored}}
	data, _ := encode(original)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is-images", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": data},
	}
	c := newFakeClient().WithRuntimeObjects(cm).Build()
	state, err := Load(context.Background(), c, "ns", "test-is")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state) != 1 || state["dest"].State != testStateMirrored {
		t.Fatalf("unexpected state: %v", state)
	}
}

// --- LoadWithExistence ---

func TestLoadWithExistence_Missing(t *testing.T) {
	c := newFakeClient().Build()
	state, exists, err := LoadWithExistence(context.Background(), c, "ns", "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatal("expected exists=false")
	}
	if state == nil || len(state) != 0 {
		t.Fatalf("expected empty non-nil state")
	}
}

func TestLoadWithExistence_Exists(t *testing.T) {
	original := ImageState{"dest": {Source: "src", State: "Pending"}}
	data, _ := encode(original)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is-images", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": data},
	}
	c := newFakeClient().WithRuntimeObjects(cm).Build()
	state, exists, err := LoadWithExistence(context.Background(), c, "ns", "test-is")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
	if len(state) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(state))
	}
}

func TestLoadWithExistence_CorruptData(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is-images", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": []byte("corrupt")},
	}
	c := newFakeClient().WithRuntimeObjects(cm).Build()
	_, exists, err := LoadWithExistence(context.Background(), c, "ns", "test-is")
	if err == nil {
		t.Fatal("expected error for corrupt data")
	}
	if !exists {
		t.Fatal("ConfigMap exists even though data is corrupt")
	}
}

// --- Save ---

func TestSave_CreatesNew(t *testing.T) {
	c := newFakeClient().Build()
	is := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is", Namespace: "ns", UID: "uid-123"},
	}
	state := ImageState{"dest": {Source: "src", State: "Pending"}}
	if err := Save(context.Background(), c, "ns", is, state); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "test-is-images"}, cm); err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "test-is" {
		t.Fatal("missing or wrong owner reference")
	}
}

func TestSave_UpdatesExisting(t *testing.T) {
	oldData, _ := encode(ImageState{"old": {Source: "old-src", State: testStateMirrored}})
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is-images", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": oldData},
	}
	c := newFakeClient().WithRuntimeObjects(existing).Build()
	is := &mirrorv1alpha1.ImageSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is", Namespace: "ns", UID: "uid-123"},
	}
	newState := ImageState{"new": {Source: "new-src", State: "Pending"}}
	if err := Save(context.Background(), c, "ns", is, newState); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "test-is-images"}, cm)
	decoded, _ := decode(cm)
	if _, ok := decoded["old"]; ok {
		t.Fatal("old entry should be gone")
	}
	if len(decoded) != 1 || decoded["new"].Source != "new-src" {
		t.Fatalf("unexpected state: %v", decoded)
	}
}

// --- SaveRaw ---

func TestSaveRaw_CreatesNew(t *testing.T) {
	c := newFakeClient().Build()
	state := ImageState{"dest": {Source: "src", State: "Pending"}}
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", state); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, cm); err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Fatal("SaveRaw should not set owner references")
	}
}

func TestSaveRaw_UpdatesExisting(t *testing.T) {
	oldData, _ := encode(ImageState{"old": {Source: "old", State: testStateMirrored}})
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "raw-cm", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": oldData},
	}
	c := newFakeClient().WithRuntimeObjects(existing).Build()
	newState := ImageState{"new": {Source: "new", State: "Pending"}}
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", newState); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, cm)
	decoded, _ := decode(cm)
	if len(decoded) != 1 || decoded["new"].Source != "new" {
		t.Fatalf("unexpected state: %v", decoded)
	}
}

// --- ImageRef helper methods ---

func TestImageEntry_HasImageSet(t *testing.T) {
	e := &ImageEntry{
		Refs: []ImageRef{
			{ImageSet: "is-a"},
			{ImageSet: "is-b"},
		},
	}
	if !e.HasImageSet("is-a") {
		t.Fatal("expected HasImageSet(is-a) = true")
	}
	if !e.HasImageSet("is-b") {
		t.Fatal("expected HasImageSet(is-b) = true")
	}
	if e.HasImageSet("is-c") {
		t.Fatal("expected HasImageSet(is-c) = false")
	}
}

func TestImageEntry_AddRef_Deduplicates(t *testing.T) {
	e := &ImageEntry{}
	e.AddRef(ImageRef{ImageSet: "is-a", Origin: OriginRelease})
	e.AddRef(ImageRef{ImageSet: "is-b", Origin: OriginOperator})
	if len(e.Refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(e.Refs))
	}
	// Update is-a ref
	e.AddRef(ImageRef{ImageSet: "is-a", Origin: OriginAdditional})
	if len(e.Refs) != 2 {
		t.Fatalf("expected 2 refs after dedup, got %d", len(e.Refs))
	}
	var ref *ImageRef
	for i := range e.Refs {
		if e.Refs[i].ImageSet == "is-a" {
			ref = &e.Refs[i]
		}
	}
	if ref == nil || ref.Origin != OriginAdditional {
		t.Fatal("AddRef should update existing ref")
	}
}

func TestImageEntry_RemoveImageSet(t *testing.T) {
	e := &ImageEntry{
		Refs: []ImageRef{
			{ImageSet: "is-a"},
			{ImageSet: "is-b"},
		},
	}
	orphaned := e.RemoveImageSet("is-a")
	if orphaned {
		t.Fatal("expected orphaned=false when one ref remains")
	}
	if len(e.Refs) != 1 || e.Refs[0].ImageSet != "is-b" {
		t.Fatalf("unexpected refs: %v", e.Refs)
	}
	orphaned = e.RemoveImageSet("is-b")
	if !orphaned {
		t.Fatal("expected orphaned=true when no refs remain")
	}
	if len(e.Refs) != 0 {
		t.Fatal("expected empty refs after removing last ref")
	}
}

func TestImageEntry_ImageSetNames(t *testing.T) {
	e := &ImageEntry{
		Refs: []ImageRef{
			{ImageSet: "is-a"},
			{ImageSet: "is-b"},
		},
	}
	names := e.ImageSetNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
}

// --- ConfigMapNameForTarget ---

func TestConfigMapNameForTarget(t *testing.T) {
	if got := ConfigMapNameForTarget("my-mt"); got != "my-mt-images" {
		t.Fatalf("expected my-mt-images, got %s", got)
	}
}

// --- CountsForImageSet ---

func TestCountsForImageSet_FiltersCorrectly(t *testing.T) {
	state := ImageState{
		"img1": {
			Source: "s1", State: testStateMirrored,
			Refs: []ImageRef{{ImageSet: "is-a"}},
		},
		"img2": {
			Source: "s2", State: "Pending",
			Refs: []ImageRef{{ImageSet: "is-b"}},
		},
		"img3": {
			Source: "s3", State: "Failed", PermanentlyFailed: true,
			Refs: []ImageRef{{ImageSet: "is-a"}, {ImageSet: "is-b"}},
		},
	}
	total, mirrored, pending, failed := CountsForImageSet(state, "is-a")
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
	if mirrored != 1 {
		t.Fatalf("expected mirrored=1, got %d", mirrored)
	}
	if pending != 0 {
		t.Fatalf("expected pending=0, got %d", pending)
	}
	if failed != 1 {
		t.Fatalf("expected failed=1, got %d", failed)
	}
}

func TestCountsForImageSet_LegacyNoRefs(t *testing.T) {
	// Legacy entries without Refs are included for all ISes
	state := ImageState{
		"img1": {Source: "s1", State: testStateMirrored},
		"img2": {Source: "s2", State: "Pending"},
	}
	total, mirrored, _, _ := CountsForImageSet(state, "any-is")
	if total != 2 || mirrored != 1 {
		t.Fatalf("expected total=2 mirrored=1, got total=%d mirrored=%d", total, mirrored)
	}
}

// --- SaveForTarget / LoadForTarget ---

func TestSaveForTarget_CreatesAndLoads(t *testing.T) {
	c := newFakeClient().Build()
	state := ImageState{"dest": {Source: "src", State: "Pending"}}
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", state); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}
	loaded, err := LoadForTarget(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadForTarget error: %v", err)
	}
	if len(loaded) != 1 || loaded["dest"].State != "Pending" {
		t.Fatalf("unexpected loaded state: %v", loaded)
	}
}
