package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

// --- SaveRaw ---

func TestSaveRaw_CreatesNew(t *testing.T) {
	c := newFakeClient().Build()
	state := ImageState{"dest": {Source: "src", State: "Pending"}}
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", state, nil, nil); err != nil {
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
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", newState, nil, nil); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, cm)
	decoded, _ := decode(cm)
	if len(decoded) != 1 || decoded["new"].Source != "new" {
		t.Fatalf("unexpected state: %v", decoded)
	}
}

// --- SharedIndex ---

func TestSharedIndex_AddSharedRef_Deduplicates(t *testing.T) {
	idx := make(SharedIndex)
	idx.AddSharedRef("dest1", "is-a")
	idx.AddSharedRef("dest1", "is-b")
	idx.AddSharedRef("dest1", "is-a") // duplicate, no-op
	if got := idx.Names("dest1"); len(got) != 2 {
		t.Fatalf("expected 2 names, got %v", got)
	}
	if !idx.IsShared("dest1") {
		t.Fatal("expected dest1 to be shared")
	}
}

func TestSharedIndex_RemoveSharedRef_DeletesEntryAtOneOrFewer(t *testing.T) {
	idx := SharedIndex{"dest1": {"is-a", "is-b", "is-c"}}
	idx.RemoveSharedRef("dest1", "is-a")
	if got := idx.Names("dest1"); len(got) != 2 {
		t.Fatalf("expected 2 remaining names, got %v", got)
	}
	if !idx.IsShared("dest1") {
		t.Fatal("expected dest1 to still be shared with 2 names")
	}

	idx.RemoveSharedRef("dest1", "is-b")
	if _, ok := idx["dest1"]; ok {
		t.Fatalf("expected dest1 entry to be deleted once only 1 name remains, got %v", idx["dest1"])
	}
	if idx.IsShared("dest1") {
		t.Fatal("expected dest1 to no longer be shared")
	}
}

func TestSharedIndex_IsShared_UnknownDest(t *testing.T) {
	idx := make(SharedIndex)
	if idx.IsShared("missing") {
		t.Fatal("expected unknown dest to not be shared")
	}
}

// --- index encode/decode roundtrip ---

func TestIndexEncodeDecode_Roundtrip(t *testing.T) {
	original := SharedIndex{
		"dest1": {"is-a", "is-b"},
		"dest2": {"is-a", "is-c", "is-d"},
	}
	data, err := encodeIndex(original)
	if err != nil {
		t.Fatalf("encodeIndex error: %v", err)
	}
	cm := &corev1.ConfigMap{BinaryData: map[string][]byte{"index.json.gz": data}}
	decoded, err := decodeIndex(cm)
	if err != nil {
		t.Fatalf("decodeIndex error: %v", err)
	}
	if len(decoded) != len(original) {
		t.Fatalf("decoded length %d != original %d", len(decoded), len(original))
	}
	for k, names := range original {
		if len(decoded[k]) != len(names) {
			t.Fatalf("decoded names for %s = %v, want %v", k, decoded[k], names)
		}
	}
}

// --- LoadIndex / SaveIndex ---

func TestSaveIndex_CreatesAndLoads(t *testing.T) {
	c := newFakeClient().Build()
	idx := SharedIndex{"dest1": {"is-a", "is-b"}}
	if err := SaveIndex(context.Background(), c, "ns", "my-mt", idx, nil, nil); err != nil {
		t.Fatalf("SaveIndex error: %v", err)
	}
	loaded, err := LoadIndex(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadIndex error: %v", err)
	}
	if !loaded.IsShared("dest1") {
		t.Fatalf("expected dest1 to be shared, got %v", loaded)
	}
}

func TestLoadIndex_MissingConfigMap(t *testing.T) {
	c := newFakeClient().Build()
	idx, err := LoadIndex(context.Background(), c, "ns", "missing-mt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx == nil || len(idx) != 0 {
		t.Fatalf("expected empty non-nil index, got %v", idx)
	}
}

func TestSaveIndex_EmptyDeletesConfigMap(t *testing.T) {
	c := newFakeClient().Build()
	idx := SharedIndex{"dest1": {"is-a", "is-b"}}
	if err := SaveIndex(context.Background(), c, "ns", "my-mt", idx, nil, nil); err != nil {
		t.Fatalf("SaveIndex error: %v", err)
	}
	if err := SaveIndex(context.Background(), c, "ns", "my-mt", SharedIndex{}, nil, nil); err != nil {
		t.Fatalf("SaveIndex (empty) error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: IndexConfigMapName("my-mt")}, cm)
	if !errors.IsNotFound(err) {
		t.Fatalf("expected index configmap to be deleted, got err=%v", err)
	}
}

func TestSaveIndex_EmptyNoopWhenAbsent(t *testing.T) {
	c := newFakeClient().Build()
	if err := SaveIndex(context.Background(), c, "ns", "my-mt", SharedIndex{}, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- ConfigMapNameForTarget / IndexConfigMapName / OrphansConfigMapName ---

func TestConfigMapNameForTarget(t *testing.T) {
	if got := ConfigMapNameForTarget("my-mt"); got != "my-mt-images" {
		t.Fatalf("expected my-mt-images, got %s", got)
	}
}

func TestIndexConfigMapName(t *testing.T) {
	if got := IndexConfigMapName("my-mt"); got != "my-mt-images-index" {
		t.Fatalf("expected my-mt-images-index, got %s", got)
	}
}

func TestOrphansConfigMapName(t *testing.T) {
	if got := OrphansConfigMapName("my-mt"); got != "my-mt-images-orphans" {
		t.Fatalf("expected my-mt-images-orphans, got %s", got)
	}
}

// --- MigrateConsolidatedToPerImageSet ---

func TestMigrate_NoLegacyConfigMap_NoOp(t *testing.T) {
	c := newFakeClient().Build()
	if err := MigrateConsolidatedToPerImageSet(context.Background(), c, "ns", "my-mt", nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMigrate_SplitsExclusiveAndSharedEntries(t *testing.T) {
	legacy := map[string]*legacyImageEntry{
		"dest-exclusive-a": {
			Source: "src-a", State: testStateMirrored,
			Refs: []legacyImageRef{{ImageSet: "is-a", Origin: OriginRelease, EntrySig: "sig-a"}},
		},
		"dest-shared": {
			Source: "src-shared", State: testStateMirrored,
			Refs: []legacyImageRef{
				{ImageSet: "is-a", Origin: OriginOperator, EntrySig: "sig-shared-a"},
				{ImageSet: "is-b", Origin: OriginOperator, EntrySig: "sig-shared-b"},
			},
		},
		"dest-orphan": {
			Source: "src-orphan", State: testStateMirrored,
			Refs: nil, // no Refs and no flat Origin => dropped, not migrated
		},
	}
	data, err := encodeGzipJSON(legacy)
	if err != nil {
		t.Fatalf("encode legacy state: %v", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-mt-images", Namespace: "ns"},
		BinaryData: map[string][]byte{"images.json.gz": data},
	}
	c := newFakeClient().WithRuntimeObjects(cm).Build()

	if err := MigrateConsolidatedToPerImageSet(context.Background(), c, "ns", "my-mt", nil, nil); err != nil {
		t.Fatalf("migrate error: %v", err)
	}

	isAState, err := Load(context.Background(), c, "ns", "is-a")
	if err != nil {
		t.Fatalf("load is-a: %v", err)
	}
	if len(isAState) != 2 {
		t.Fatalf("expected 2 entries for is-a, got %d: %v", len(isAState), isAState)
	}
	if isAState["dest-exclusive-a"].EntrySig != "sig-a" {
		t.Fatalf("unexpected exclusive entry: %v", isAState["dest-exclusive-a"])
	}
	if isAState["dest-shared"].EntrySig != "sig-shared-a" {
		t.Fatalf("unexpected shared entry for is-a: %v", isAState["dest-shared"])
	}

	isBState, err := Load(context.Background(), c, "ns", "is-b")
	if err != nil {
		t.Fatalf("load is-b: %v", err)
	}
	if len(isBState) != 1 || isBState["dest-shared"].EntrySig != "sig-shared-b" {
		t.Fatalf("unexpected is-b state: %v", isBState)
	}

	idx, err := LoadIndex(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	if !idx.IsShared("dest-shared") {
		t.Fatalf("expected dest-shared to be in the index, got %v", idx)
	}
	if idx.IsShared("dest-exclusive-a") {
		t.Fatalf("expected dest-exclusive-a to NOT be in the index, got %v", idx)
	}

	// Legacy consolidated ConfigMap must be gone after migration.
	remaining := &corev1.ConfigMap{}
	getErr := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "my-mt-images"}, remaining)
	if !errors.IsNotFound(getErr) {
		t.Fatalf("expected legacy configmap to be deleted, got err=%v", getErr)
	}
}
