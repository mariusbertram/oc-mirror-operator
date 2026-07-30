package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// v1Blob encodes state in the legacy schema-v1 format (gzip-compressed JSON
// map) exactly as older versions wrote it, for backward-compat decode tests.
func v1Blob(t *testing.T, state ImageState) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal v1 state: %v", err)
	}
	return mustGzip(raw)
}

func newFakeClient() *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = mirrorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme)
}

func entriesEqual(t *testing.T, key string, got, want *ImageEntry) {
	t.Helper()
	if got.Source != want.Source || got.State != want.State || got.RetryCount != want.RetryCount ||
		got.LastError != want.LastError || got.Origin != want.Origin || got.EntrySig != want.EntrySig ||
		got.OriginRef != want.OriginRef || got.PermanentlyFailed != want.PermanentlyFailed {
		t.Fatalf("entry mismatch for %s:\n got  %+v\n want %+v", key, got, want)
	}
	if len(got.Refs) != len(want.Refs) {
		t.Fatalf("refs length mismatch for %s: got %d want %d", key, len(got.Refs), len(want.Refs))
	}
	for i := range want.Refs {
		if got.Refs[i] != want.Refs[i] {
			t.Fatalf("ref %d mismatch for %s: got %+v want %+v", i, key, got.Refs[i], want.Refs[i])
		}
	}
}

func statesEqual(t *testing.T, got, want ImageState) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("state length mismatch: got %d want %d", len(got), len(want))
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("missing key %s", k)
		}
		entriesEqual(t, k, g, w)
	}
}

func sampleState() ImageState {
	return ImageState{
		"registry.example.com/repo@sha256:abc": {
			Source:            "quay.io/source@sha256:abc",
			State:             testStateMirrored,
			Origin:            OriginRelease,
			EntrySig:          "sig123",
			OriginRef:         "stable-4.14 [amd64]",
			PermanentlyFailed: false,
			Refs: []ImageRef{
				{ImageSet: "is-a", Origin: OriginRelease, EntrySig: "sig123", OriginRef: "stable-4.14 [amd64]"},
				{ImageSet: "is-b", Origin: OriginOperator, EntrySig: "sig456", OriginRef: "catalog [pkg1]"},
			},
		},
		"registry.example.com/repo2:v1.0": {
			Source:     "docker.io/library/alpine:3.18",
			State:      "Pending",
			RetryCount: 3,
			LastError:  "timeout",
			Origin:     OriginAdditional,
			Refs: []ImageRef{
				{ImageSet: "is-a", Origin: OriginAdditional},
			},
		},
		"registry.example.com/repo3:v2.0": {
			Source:            "quay.io/other/thing:v2.0",
			State:             "Failed",
			RetryCount:        10,
			LastError:         "manifest unknown",
			PermanentlyFailed: true,
		},
	}
}

// --- ConfigMapName / ConfigMapNameForTarget / TargetNameFromStateCM ---

func TestConfigMapName(t *testing.T) {
	if got := ConfigMapName("my-imageset"); got != "my-imageset-images" {
		t.Fatalf("expected my-imageset-images, got %s", got)
	}
	if got := ConfigMapName(""); got != "-images" {
		t.Fatalf("expected -images, got %s", got)
	}
}

func TestConfigMapNameForTarget(t *testing.T) {
	if got := ConfigMapNameForTarget("my-mt"); got != "my-mt-images" {
		t.Fatalf("expected my-mt-images, got %s", got)
	}
}

func TestTargetNameFromStateCM(t *testing.T) {
	cases := []struct {
		in     string
		wantMT string
		wantOK bool
	}{
		{"my-mt-images", "my-mt", true},
		{"my-mt-images-s0", "my-mt", true},
		{"my-mt-images-s17", "my-mt", true},
		{"my-mt-images-sx", "", false},
		{"my-mt-images-s", "", false},
		{"unrelated", "", false},
		{"my-mt-imagestore", "", false},
	}
	for _, tc := range cases {
		mt, ok := TargetNameFromStateCM(tc.in)
		if ok != tc.wantOK || mt != tc.wantMT {
			t.Fatalf("TargetNameFromStateCM(%q) = (%q, %v), want (%q, %v)", tc.in, mt, ok, tc.wantMT, tc.wantOK)
		}
	}
}

func TestShardCountFromMeta_Bounds(t *testing.T) {
	cases := []struct {
		shards string
		wantN  int
		wantOK bool
	}{
		{"8", 8, true},
		{"1", 1, true},
		{"512", 512, true},
		{"513", 0, false},
		{"0", 0, false},
		{"-3", 0, false},
		{"1000000000000", 0, false},
		{"nan", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		cm := &corev1.ConfigMap{Data: map[string]string{metaKeyShards: tc.shards}}
		n, ok := shardCountFromMeta(cm)
		if n != tc.wantN || ok != tc.wantOK {
			t.Fatalf("shardCountFromMeta(shards=%q) = (%d, %v), want (%d, %v)", tc.shards, n, ok, tc.wantN, tc.wantOK)
		}
	}
}

func TestShardIndex_InRange(t *testing.T) {
	for _, n := range []int{1, 2, 8, maxShardCount, maxShardCount + 100} {
		for _, dest := range []string{"", "a", "registry.example.com/repo@sha256:abc"} {
			i := shardIndex(dest, n)
			bound := n
			if bound > maxShardCount {
				bound = maxShardCount
			}
			if i < 0 || i >= bound {
				t.Fatalf("shardIndex(%q, %d) = %d out of range [0,%d)", dest, n, i, bound)
			}
		}
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

// --- v2 codec ---

func TestEncodeDecodeV2_Roundtrip(t *testing.T) {
	original := sampleState()
	data, err := encodeV2(original)
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encoded data is empty")
	}
	decoded, err := decodeV2(data)
	if err != nil {
		t.Fatalf("decodeV2 error: %v", err)
	}
	statesEqual(t, decoded, original)
}

func TestEncodeV2_EmptyState(t *testing.T) {
	data, err := encodeV2(ImageState{})
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	decoded, err := decodeV2(data)
	if err != nil {
		t.Fatalf("decodeV2 error: %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected empty decoded state, got %d entries", len(decoded))
	}
}

func TestEncodeV2_Deterministic(t *testing.T) {
	state := sampleState()
	a, err := encodeV2(state)
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	b, err := encodeV2(state)
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("encodeV2 is not deterministic for identical state")
	}
}

func TestEncodeV2_TruncatesLastError(t *testing.T) {
	long := strings.Repeat("x", maxLastErrorLen*3)
	state := ImageState{"dest": {Source: "src", State: "Failed", LastError: long}}
	data, err := encodeV2(state)
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	decoded, err := decodeV2(data)
	if err != nil {
		t.Fatalf("decodeV2 error: %v", err)
	}
	got := decoded["dest"].LastError
	if len(got) > maxLastErrorLen+len("…[truncated]") {
		t.Fatalf("LastError not truncated: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("expected truncation marker, got suffix %q", got[len(got)-20:])
	}
}

func TestEncodeV2_InternsSharedRefTuples(t *testing.T) {
	// Many images from the same spec entry must not repeat the provenance
	// tuple per image: v2 must be much smaller than v1 for this shape.
	sig := strings.Repeat("ab12", 16) // 64 hex chars, like a real entry sig
	ref := "registry.redhat.io/redhat/redhat-operator-index:v4.21 [pkg-one, pkg-two]"
	state := make(ImageState, 200)
	for i := 0; i < 200; i++ {
		dest := "registry.example.com/redhat/repo" + strings.Repeat("x", i%7) + "@sha256:" + strings.Repeat("0", 61) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + "f"
		state[dest] = &ImageEntry{
			Source: "registry.redhat.io/redhat/repo@sha256:" + strings.Repeat("0", 61) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + "f",
			State:  testStateMirrored,
			Refs: []ImageRef{
				{ImageSet: "is-a", Origin: OriginOperator, EntrySig: sig, OriginRef: ref},
				{ImageSet: "is-b", Origin: OriginOperator, EntrySig: sig, OriginRef: ref},
			},
		}
	}
	v2, err := encodeV2(state)
	if err != nil {
		t.Fatalf("encodeV2 error: %v", err)
	}
	v1 := v1Blob(t, state)
	if len(v2) >= len(v1) {
		t.Fatalf("expected v2 (%d bytes) to be smaller than v1 (%d bytes)", len(v2), len(v1))
	}
	decoded, err := decodeV2(v2)
	if err != nil {
		t.Fatalf("decodeV2 error: %v", err)
	}
	statesEqual(t, decoded, state)
}

func TestDecodeV2_CorruptGroupIndex(t *testing.T) {
	doc := v2Doc{Version: 2, Images: map[string]v2Entry{"dest": {Source: "src", State: "Pending", Groups: []int{5}}}}
	raw, _ := json.Marshal(doc)
	if _, err := decodeV2(mustGzip(raw)); err == nil {
		t.Fatal("expected error for out-of-range group index")
	}
}

func TestDecodeV2_UnsupportedVersion(t *testing.T) {
	raw, _ := json.Marshal(v2Doc{Version: 3, Images: map[string]v2Entry{}})
	if _, err := decodeV2(mustGzip(raw)); err == nil {
		t.Fatal("expected error for unsupported document version")
	}
}

// --- decode (single-CM, all formats) ---

func TestDecode_V1Gzip(t *testing.T) {
	original := sampleState()
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{blobKeyV1: v1Blob(t, original)},
	}
	decoded, err := decode(cm)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	statesEqual(t, decoded, original)
}

func TestDecode_PlainJSON(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			blobKeyV1Plain: `{"img1":{"source":"src1","state":"Mirrored"}}`,
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
			blobKeyV1: []byte("not-actually-gzip-data"),
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
			blobKeyV1Plain: "{not-valid-json",
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
		BinaryData: map[string][]byte{blobKeyV1: mustGzip([]byte("{broken"))},
	}
	if _, err := decode(cm); err == nil {
		t.Fatal("expected error for valid gzip but corrupt JSON")
	}
}

func TestDecode_CorruptV2Gzip(t *testing.T) {
	cm := &corev1.ConfigMap{
		BinaryData: map[string][]byte{blobKeyV2: []byte("not-gzip")},
	}
	if _, err := decode(cm); err == nil {
		t.Fatal("expected error for corrupt v2 gzip")
	}
}

// --- Load / LoadByConfigMapName ---

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

func TestLoad_ExistingV1ConfigMap(t *testing.T) {
	original := ImageState{"dest": {Source: "src", State: testStateMirrored}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-is-images", Namespace: "ns"},
		BinaryData: map[string][]byte{blobKeyV1: v1Blob(t, original)},
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
	if _, ok := cm.BinaryData[blobKeyV2]; !ok {
		t.Fatal("SaveRaw should write the v2 document key")
	}
	loaded, err := LoadByConfigMapName(context.Background(), c, "ns", "raw-cm")
	if err != nil {
		t.Fatalf("LoadByConfigMapName error: %v", err)
	}
	if len(loaded) != 1 || loaded["dest"].State != "Pending" {
		t.Fatalf("unexpected loaded state: %v", loaded)
	}
}

func TestSaveRaw_UpdatesExistingV1(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "raw-cm", Namespace: "ns"},
		BinaryData: map[string][]byte{blobKeyV1: v1Blob(t, ImageState{"old": {Source: "old", State: testStateMirrored}})},
	}
	c := newFakeClient().WithRuntimeObjects(existing).Build()
	newState := ImageState{"new": {Source: "new", State: "Pending"}}
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", newState, nil, nil); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	cm := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, cm)
	if _, ok := cm.BinaryData[blobKeyV1]; ok {
		t.Fatal("legacy v1 blob should be dropped on update")
	}
	decoded, err := decode(cm)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(decoded) != 1 || decoded["new"].Source != "new" {
		t.Fatalf("unexpected state: %v", decoded)
	}
}

func TestSaveRaw_SkipsWriteWhenUnchanged(t *testing.T) {
	c := newFakeClient().Build()
	state := sampleState()
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", state, nil, nil); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	before := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, before)
	if err := SaveRaw(context.Background(), c, "ns", "raw-cm", state, nil, nil); err != nil {
		t.Fatalf("SaveRaw error: %v", err)
	}
	after := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "raw-cm"}, after)
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatalf("unchanged state must not be rewritten (rv %s -> %s)", before.ResourceVersion, after.ResourceVersion)
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

// --- SaveForTarget / LoadForTarget (sharded store) ---

func TestSaveForTarget_CreatesShardedStoreAndLoads(t *testing.T) {
	c := newFakeClient().Build()
	state := sampleState()
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", state, nil, nil); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}

	meta := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "my-mt-images"}, meta); err != nil {
		t.Fatalf("meta ConfigMap not found: %v", err)
	}
	if meta.Data[metaKeySchemaVersion] != schemaVersionV2 {
		t.Fatalf("expected schemaVersion=2, got %q", meta.Data[metaKeySchemaVersion])
	}
	if n, ok := shardCountFromMeta(meta); !ok || n != DefaultShardCount {
		t.Fatalf("expected shard count %d, got %d (ok=%v)", DefaultShardCount, n, ok)
	}
	if len(meta.BinaryData) != 0 {
		t.Fatal("meta ConfigMap must not carry a state blob")
	}

	loaded, err := LoadForTarget(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadForTarget error: %v", err)
	}
	statesEqual(t, loaded, state)
}

func TestSaveForTarget_EmptyShardsNotCreated(t *testing.T) {
	c := newFakeClient().Build()
	// A single entry occupies exactly one shard; the other shard CMs must
	// not be created.
	state := ImageState{"only-dest": {Source: "src", State: "Pending"}}
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", state, nil, nil); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}
	existing := 0
	for i := 0; i < DefaultShardCount; i++ {
		cm := &corev1.ConfigMap{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: shardCMName("my-mt-images", i)}, cm); err == nil {
			existing++
		}
	}
	if existing != 1 {
		t.Fatalf("expected exactly 1 shard ConfigMap, found %d", existing)
	}
	loaded, err := LoadForTarget(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadForTarget error: %v", err)
	}
	statesEqual(t, loaded, state)
}

func TestSaveForTarget_OnlyDirtyShardsRewritten(t *testing.T) {
	c := newFakeClient().Build()
	// Enough entries to populate all shards.
	state := make(ImageState)
	for i := 0; i < 100; i++ {
		n := strconv.Itoa(i)
		state["registry.example.com/repo"+n+":v1"] = &ImageEntry{Source: "src" + n, State: "Pending"}
	}
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", state, nil, nil); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}

	rvBefore := shardResourceVersions(t, c, "my-mt-images")

	// Flip a single entry and save again.
	const changed = "registry.example.com/repo7:v1"
	state[changed].State = testStateMirrored
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", state, nil, nil); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}

	rvAfter := shardResourceVersions(t, c, "my-mt-images")
	changedShard := shardIndex(changed, DefaultShardCount)
	for i := 0; i < DefaultShardCount; i++ {
		if i == changedShard {
			if rvBefore[i] == rvAfter[i] {
				t.Fatalf("shard %d holds the changed entry but was not rewritten", i)
			}
			continue
		}
		if rvBefore[i] != rvAfter[i] {
			t.Fatalf("shard %d was rewritten although unchanged (rv %s -> %s)", i, rvBefore[i], rvAfter[i])
		}
	}
}

func TestSaveForTarget_MigratesV1Blob(t *testing.T) {
	original := sampleState()
	v1CM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-mt-images", Namespace: "ns"},
		BinaryData: map[string][]byte{blobKeyV1: v1Blob(t, original)},
	}
	c := newFakeClient().WithRuntimeObjects(v1CM).Build()

	// v1 store loads as-is.
	loaded, err := LoadForTarget(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadForTarget (v1) error: %v", err)
	}
	statesEqual(t, loaded, original)

	// First save converts to the sharded layout and drops the v1 blob.
	if err := SaveForTarget(context.Background(), c, "ns", "my-mt", loaded, nil, nil); err != nil {
		t.Fatalf("SaveForTarget error: %v", err)
	}
	meta := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "my-mt-images"}, meta); err != nil {
		t.Fatalf("meta ConfigMap not found: %v", err)
	}
	if _, ok := meta.BinaryData[blobKeyV1]; ok {
		t.Fatal("v1 blob should be dropped after migration")
	}
	if _, ok := shardCountFromMeta(meta); !ok {
		t.Fatal("meta ConfigMap should record the shard count after migration")
	}
	reloaded, err := LoadForTarget(context.Background(), c, "ns", "my-mt")
	if err != nil {
		t.Fatalf("LoadForTarget (v2) error: %v", err)
	}
	statesEqual(t, reloaded, original)
}

// shardResourceVersions returns the ResourceVersion of every shard ConfigMap
// (empty string for shards that do not exist).
func shardResourceVersions(t *testing.T, c client.Client, base string) []string {
	t.Helper()
	rvs := make([]string, DefaultShardCount)
	for i := 0; i < DefaultShardCount; i++ {
		cm := &corev1.ConfigMap{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: shardCMName(base, i)}, cm); err == nil {
			rvs[i] = cm.ResourceVersion
		}
	}
	return rvs
}
