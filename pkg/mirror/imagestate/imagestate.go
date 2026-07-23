// Package imagestate manages per-image mirroring state via ConfigMaps,
// avoiding size limits of Kubernetes CR status fields.
//
// Storage layout (schema v2): each MirrorTarget gets a small meta ConfigMap
// "<mt>-images" (schema version + shard count) plus DefaultShardCount shard
// ConfigMaps "<mt>-images-s<N>", each holding a self-contained gzip-compressed
// document for the destinations that hash into it. Within a document the
// per-spec-entry provenance tuples (imageSet, origin, entrySig, originRef) —
// identical for every image produced by the same spec entry — are interned in
// a group table and referenced by index, which removes the dominant source of
// duplication. Saves are deterministic and skip shards whose encoded bytes
// are unchanged, so a single image flipping state rewrites one shard instead
// of the whole store.
//
// Single-ConfigMap stores written by SaveRaw (cleanup snapshots) use the same
// v2 document under one key. The legacy schema-v1 layout (one gzip-compressed
// JSON map under "images.json.gz", or plain "images.json") is still decoded;
// the first SaveForTarget converts a v1 store to the sharded v2 layout.
package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ImageOrigin identifies which collector source produced a given imagestate
// entry. Used for ownership tracking when multiple writers (controller +
// manager) update the same ConfigMap.
type ImageOrigin string

const (
	// OriginRelease marks images extracted from OCP release payloads
	// (platform.channels) and KubeVirt container disks.
	OriginRelease ImageOrigin = "release"
	// OriginOperator marks bundle and related images extracted from operator
	// catalogs (mirror.operators[]).
	OriginOperator ImageOrigin = "operator"
	// OriginAdditional marks images explicitly enumerated via
	// mirror.additionalImages.
	OriginAdditional ImageOrigin = "additional"
	// OriginHelm marks images extracted from rendered Helm charts
	// (mirror.helm.repositories[].charts[]).
	OriginHelm ImageOrigin = "helm"
)

const (
	// blobKeyV1 / blobKeyV1Plain are the legacy schema-v1 single-blob keys.
	blobKeyV1      = "images.json.gz"
	blobKeyV1Plain = "images.json"
	// blobKeyV2 holds a self-contained schema-v2 document: the whole state
	// for single-ConfigMap stores (SaveRaw snapshots), or one shard of a
	// sharded per-MirrorTarget store.
	blobKeyV2 = "state.v2.json.gz"
	// metaKeySchemaVersion / metaKeyShards mark the meta ConfigMap of a
	// sharded v2 store and record how many shard ConfigMaps belong to it.
	metaKeySchemaVersion = "schemaVersion"
	metaKeyShards        = "shards"

	schemaVersionV2 = "2"

	// DefaultShardCount is the shard count new sharded stores are created
	// with. Existing stores keep the count recorded in their meta ConfigMap,
	// so changing this constant never re-shards existing state.
	DefaultShardCount = 8

	// maxLastErrorLen caps persisted LastError strings so registry error
	// bursts cannot blow up the store size.
	maxLastErrorLen = 512
)

// ImageRef holds per-ImageSet metadata for an entry in the consolidated state.
// Multiple ImageSets can reference the same destination (shared image); each
// gets its own Ref so Origin/EntrySig/OriginRef are tracked independently.
type ImageRef struct {
	ImageSet  string      `json:"imageSet"`
	Origin    ImageOrigin `json:"origin,omitempty"`
	EntrySig  string      `json:"entrySig,omitempty"`
	OriginRef string      `json:"originRef,omitempty"`
}

// ImageEntry tracks the mirroring state of a single image.
// The destination image reference is the map key in ImageState.
type ImageEntry struct {
	Source     string `json:"source"`
	State      string `json:"state"` // Pending | Mirrored | Failed
	LastError  string `json:"lastError,omitempty"`
	RetryCount int    `json:"retryCount,omitempty"`
	// Origin records which collector produced this entry. Empty for entries
	// written by older controller versions (treated as OriginRelease for
	// backward compatibility during migration).
	// Deprecated: use Refs for new code; kept for backward-compat deserialization.
	Origin ImageOrigin `json:"origin,omitempty"`
	// EntrySig is the per-spec-entry signature that produced this entry.
	// Deprecated: use Refs for new code; kept for backward-compat deserialization.
	EntrySig string `json:"entrySig,omitempty"`
	// OriginRef is a human-readable label describing which spec entry produced
	// this entry. Deprecated: use Refs for new code; kept for backward-compat.
	OriginRef string `json:"originRef,omitempty"`
	// PermanentlyFailed is set to true when the image has exhausted its initial
	// retry budget (RetryCount >= 10). Once set it is never cleared — even when
	// the image is reset to Pending for a drift-check retry attempt. This flag
	// is used to keep the catalog-build gate open and to surface the image in
	// failedImageDetails regardless of the current retry state.
	PermanentlyFailed bool `json:"permanentlyFailed,omitempty"`
	// Refs holds per-ImageSet metadata. Multiple ImageSets can reference the
	// same destination (shared image); each gets its own Ref with independent
	// Origin/EntrySig/OriginRef. This replaces the flat Origin/EntrySig/OriginRef
	// fields for the consolidated per-MirrorTarget state.
	Refs []ImageRef `json:"refs,omitempty"`
}

// HasImageSet reports whether any Ref in e references the given ImageSet name.
func (e *ImageEntry) HasImageSet(name string) bool {
	for _, r := range e.Refs {
		if r.ImageSet == name {
			return true
		}
	}
	return false
}

// AddRef adds a Ref to e, deduplicating by ImageSet name (last write wins for
// Origin/EntrySig/OriginRef when the ImageSet already has a Ref).
func (e *ImageEntry) AddRef(ref ImageRef) {
	for i, r := range e.Refs {
		if r.ImageSet == ref.ImageSet {
			e.Refs[i] = ref
			return
		}
	}
	e.Refs = append(e.Refs, ref)
}

// RemoveImageSet removes the Ref for the given ImageSet from e.Refs.
// Returns true if no Refs remain after removal (the entry is now orphaned).
func (e *ImageEntry) RemoveImageSet(name string) bool {
	out := e.Refs[:0]
	for _, r := range e.Refs {
		if r.ImageSet != name {
			out = append(out, r)
		}
	}
	e.Refs = out
	return len(e.Refs) == 0
}

// ImageSetNames returns the names of all ImageSets that reference this entry.
func (e *ImageEntry) ImageSetNames() []string {
	names := make([]string, 0, len(e.Refs))
	for _, r := range e.Refs {
		names = append(names, r.ImageSet)
	}
	return names
}

// ImageState maps destination image reference → ImageEntry.
type ImageState map[string]*ImageEntry

// Deprecated: ConfigMapName returns the per-ImageSet ConfigMap name.
// Use ConfigMapNameForTarget for the consolidated per-MirrorTarget state store.
func ConfigMapName(imageSetName string) string {
	return imageSetName + "-images"
}

// ConfigMapNameForTarget returns the consolidated ConfigMap name for a
// MirrorTarget. This is the meta ConfigMap of the sharded per-MirrorTarget
// state store (and the legacy single-blob store during migration); it
// replaces the per-ImageSet "<imageset>-images" ConfigMaps.
func ConfigMapNameForTarget(mtName string) string {
	return mtName + "-images"
}

// shardCMName returns the name of shard i of the sharded store rooted at the
// meta ConfigMap name base.
func shardCMName(base string, i int) string {
	return fmt.Sprintf("%s-s%d", base, i)
}

// TargetNameFromStateCM maps a state-store ConfigMap name — meta
// ("<mt>-images") or shard ("<mt>-images-s<N>") — back to the MirrorTarget
// name. ok is false when the name does not belong to a state store.
func TargetNameFromStateCM(cmName string) (mtName string, ok bool) {
	const suffix = "-images"
	if strings.HasSuffix(cmName, suffix) {
		return strings.TrimSuffix(cmName, suffix), true
	}
	idx := strings.LastIndex(cmName, suffix+"-s")
	if idx < 0 {
		return "", false
	}
	tail := cmName[idx+len(suffix)+2:]
	if tail == "" {
		return "", false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return cmName[:idx], true
}

// shardIndex assigns a destination to one of n shards. The assignment must be
// stable across versions — it determines which ConfigMap an entry lives in.
func shardIndex(dest string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(dest))
	return int(h.Sum32() % uint32(n)) //nolint:gosec // n is a small positive shard count
}

// Counts returns aggregate counts across the ImageState.
//   - mirrored: State == "Mirrored"
//   - failed:   PermanentlyFailed == true AND State != "Mirrored"
//     (covers both "Failed" at rest and "Pending" while being retried)
//   - pending:  everything else (State == "Pending", not permanently failed)
func Counts(state ImageState) (total, mirrored, pending, failed int) {
	total = len(state)
	for _, e := range state {
		switch {
		case e.State == "Mirrored":
			mirrored++
		case e.PermanentlyFailed:
			failed++
		default:
			pending++
		}
	}
	return
}

// Deprecated: Load reads from a per-ImageSet ConfigMap.
// Use LoadForTarget for the consolidated per-MirrorTarget state store.
func Load(ctx context.Context, c client.Client, namespace, imageSetName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapName(imageSetName))
}

// LoadByConfigMapName reads the ImageState rooted at the ConfigMap with the
// given name: a sharded v2 store (meta ConfigMap + shards), a single-ConfigMap
// v2 document, or a legacy v1 blob. Returns an empty ImageState (not nil) if
// the ConfigMap does not exist.
func LoadByConfigMapName(ctx context.Context, c client.Client, namespace, cmName string) (ImageState, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return make(ImageState), nil
		}
		return nil, fmt.Errorf("get image state configmap: %w", err)
	}
	if n, ok := shardCountFromMeta(cm); ok {
		return loadShards(ctx, c, namespace, cmName, n)
	}
	return decode(cm)
}

// shardCountFromMeta reports whether cm is the meta ConfigMap of a sharded
// store and, if so, its shard count.
func shardCountFromMeta(cm *corev1.ConfigMap) (int, bool) {
	n, err := strconv.Atoi(cm.Data[metaKeyShards])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// loadShards reads and merges all shard ConfigMaps of a sharded store.
// Missing shards are treated as empty (they are only created once non-empty).
func loadShards(ctx context.Context, c client.Client, namespace, base string, n int) (ImageState, error) {
	state := make(ImageState)
	for i := 0; i < n; i++ {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: shardCMName(base, i)}, cm)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get image state shard %d: %w", i, err)
		}
		shard, err := decode(cm)
		if err != nil {
			return nil, fmt.Errorf("decode image state shard %d: %w", i, err)
		}
		for dest, entry := range shard {
			state[dest] = entry
		}
	}
	return state, nil
}

// LoadForTarget reads the consolidated ImageState from the per-MirrorTarget
// state store. Returns an empty ImageState (not nil) if the store does not
// exist yet.
func LoadForTarget(ctx context.Context, c client.Client, namespace, mtName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapNameForTarget(mtName))
}

// SaveForTarget writes the consolidated ImageState to the sharded
// per-MirrorTarget store rooted at "<mtName>-images". Shards whose encoded
// content is unchanged are skipped, so steady-state saves only rewrite the
// shards that actually changed. A legacy v1 single-blob store is converted to
// the sharded layout on the first save (shards are written before the meta
// ConfigMap flips, so readers never see a v2 meta without its shards).
func SaveForTarget(ctx context.Context, c client.Client, namespace, mtName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	base := ConfigMapNameForTarget(mtName)

	meta := &corev1.ConfigMap{}
	metaErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: base}, meta)
	if metaErr != nil && !errors.IsNotFound(metaErr) {
		return fmt.Errorf("get image state meta configmap: %w", metaErr)
	}
	metaExists := metaErr == nil

	// Reuse the shard count recorded in the meta ConfigMap so the shard
	// assignment of existing entries stays stable.
	n := DefaultShardCount
	if metaExists {
		if v, ok := shardCountFromMeta(meta); ok {
			n = v
		}
	}

	shards := make([]ImageState, n)
	for i := range shards {
		shards[i] = make(ImageState)
	}
	for dest, entry := range state {
		shards[shardIndex(dest, n)][dest] = entry
	}

	for i, shard := range shards {
		data, err := encodeV2(shard)
		if err != nil {
			return fmt.Errorf("encode image state shard %d: %w", i, err)
		}
		// Empty shards are only written when their ConfigMap already exists
		// (to overwrite stale entries); otherwise they are never created.
		if err := writeStateCM(ctx, c, namespace, shardCMName(base, i), data, owner, scheme, len(shard) > 0); err != nil {
			return fmt.Errorf("save image state shard %d: %w", i, err)
		}
	}

	// The meta ConfigMap is static (schema version + shard count): it is
	// written once on creation and once when a legacy v1 blob is converted
	// (the update drops the blob, completing the migration).
	desiredShards := strconv.Itoa(n)
	if metaExists && len(meta.BinaryData) == 0 &&
		meta.Data[metaKeySchemaVersion] == schemaVersionV2 && meta.Data[metaKeyShards] == desiredShards {
		return nil
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: base, Namespace: namespace},
		Data: map[string]string{
			metaKeySchemaVersion: schemaVersionV2,
			metaKeyShards:        desiredShards,
		},
	}
	if owner != nil && scheme != nil {
		if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}
	}
	if !metaExists {
		return c.Create(ctx, cm)
	}
	cm.ResourceVersion = meta.ResourceVersion
	return c.Update(ctx, cm)
}

// SaveRaw writes the ImageState as a single self-contained v2 document to a
// ConfigMap with the given name. Used for cleanup snapshot state that is not
// part of the sharded per-MirrorTarget store. If owner and scheme are
// provided, a ControllerReference is set on the ConfigMap.
func SaveRaw(ctx context.Context, c client.Client, namespace, cmName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	data, err := encodeV2(state)
	if err != nil {
		return fmt.Errorf("encode image state: %w", err)
	}
	return writeStateCM(ctx, c, namespace, cmName, data, owner, scheme, true)
}

// writeStateCM creates or updates cmName so that its only payload is
// BinaryData[blobKeyV2] == data. The API write is skipped when the stored
// bytes are already identical (encoding is deterministic, so equal state
// yields equal bytes). createIfMissing=false suppresses creation for empty
// shards.
func writeStateCM(ctx context.Context, c client.Client, namespace, cmName string, data []byte, owner metav1.Object, scheme *runtime.Scheme, createIfMissing bool) error {
	existing := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)
	if getErr != nil && !errors.IsNotFound(getErr) {
		return getErr
	}
	exists := getErr == nil

	if exists && len(existing.Data) == 0 && len(existing.BinaryData) == 1 &&
		bytes.Equal(existing.BinaryData[blobKeyV2], data) {
		return nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: namespace},
		BinaryData: map[string][]byte{blobKeyV2: data},
	}
	if owner != nil && scheme != nil {
		if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}
	}
	if !exists {
		if !createIfMissing {
			return nil
		}
		return c.Create(ctx, cm)
	}
	cm.ResourceVersion = existing.ResourceVersion
	return c.Update(ctx, cm)
}

// --- schema v2 codec ---

// v2Group is one interned provenance tuple. Entries reference groups by index.
// A group with an empty ImageSet interns the deprecated flat
// Origin/EntrySig/OriginRef triple of legacy entries.
type v2Group struct {
	ImageSet  string      `json:"is,omitempty"`
	Origin    ImageOrigin `json:"o,omitempty"`
	EntrySig  string      `json:"sig,omitempty"`
	OriginRef string      `json:"ref,omitempty"`
}

// v2Entry is the on-wire form of ImageEntry with provenance replaced by group
// indices.
type v2Entry struct {
	Source     string `json:"s,omitempty"`
	State      string `json:"st,omitempty"`
	LastError  string `json:"le,omitempty"`
	RetryCount int    `json:"rc,omitempty"`
	PermFailed bool   `json:"pf,omitempty"`
	// Flat indexes the group interning the deprecated flat triple; nil when
	// the entry has none.
	Flat *int `json:"f,omitempty"`
	// Groups indexes one group per ImageRef in Refs.
	Groups []int `json:"g,omitempty"`
}

// v2Doc is one self-contained schema-v2 document: a full state (single-CM
// stores) or one shard of a sharded store.
type v2Doc struct {
	Version int                `json:"v"`
	Groups  []v2Group          `json:"g,omitempty"`
	Images  map[string]v2Entry `json:"i"`
}

func flatGroup(e *ImageEntry) (v2Group, bool) {
	if e.Origin == "" && e.EntrySig == "" && e.OriginRef == "" {
		return v2Group{}, false
	}
	return v2Group{Origin: e.Origin, EntrySig: e.EntrySig, OriginRef: e.OriginRef}, true
}

func truncateError(s string) string {
	if len(s) <= maxLastErrorLen {
		return s
	}
	return s[:maxLastErrorLen] + "…[truncated]"
}

// encodeV2 serializes state as a gzip-compressed v2 document. The output is
// deterministic for logically identical state (sorted group table, JSON map
// keys sorted by encoding/json, fixed gzip parameters), which writeStateCM
// relies on to skip unchanged shards.
func encodeV2(state ImageState) ([]byte, error) {
	groupSet := make(map[v2Group]struct{})
	for _, e := range state {
		for _, r := range e.Refs {
			groupSet[v2Group(r)] = struct{}{}
		}
		if g, ok := flatGroup(e); ok {
			groupSet[g] = struct{}{}
		}
	}
	groups := make([]v2Group, 0, len(groupSet))
	for g := range groupSet {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.ImageSet != b.ImageSet {
			return a.ImageSet < b.ImageSet
		}
		if a.Origin != b.Origin {
			return a.Origin < b.Origin
		}
		if a.EntrySig != b.EntrySig {
			return a.EntrySig < b.EntrySig
		}
		return a.OriginRef < b.OriginRef
	})
	index := make(map[v2Group]int, len(groups))
	for i, g := range groups {
		index[g] = i
	}

	doc := v2Doc{Version: 2, Groups: groups, Images: make(map[string]v2Entry, len(state))}
	for dest, e := range state {
		ve := v2Entry{
			Source:     e.Source,
			State:      e.State,
			LastError:  truncateError(e.LastError),
			RetryCount: e.RetryCount,
			PermFailed: e.PermanentlyFailed,
		}
		if g, ok := flatGroup(e); ok {
			i := index[g]
			ve.Flat = &i
		}
		for _, r := range e.Refs {
			ve.Groups = append(ve.Groups, index[v2Group(r)])
		}
		doc.Images[dest] = ve
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(raw); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func groupAt(groups []v2Group, i int) (v2Group, error) {
	if i < 0 || i >= len(groups) {
		return v2Group{}, fmt.Errorf("group index %d out of range (%d groups)", i, len(groups))
	}
	return groups[i], nil
}

func decodeV2(data []byte) (ImageState, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image state v2: gzip reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	var doc v2Doc
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode image state v2: json decode: %w", err)
	}
	if doc.Version != 2 {
		return nil, fmt.Errorf("decode image state v2: unsupported document version %d", doc.Version)
	}
	state := make(ImageState, len(doc.Images))
	for dest, ve := range doc.Images {
		e := &ImageEntry{
			Source:            ve.Source,
			State:             ve.State,
			LastError:         ve.LastError,
			RetryCount:        ve.RetryCount,
			PermanentlyFailed: ve.PermFailed,
		}
		if ve.Flat != nil {
			g, err := groupAt(doc.Groups, *ve.Flat)
			if err != nil {
				return nil, fmt.Errorf("decode image state v2: entry %s: %w", dest, err)
			}
			e.Origin, e.EntrySig, e.OriginRef = g.Origin, g.EntrySig, g.OriginRef
		}
		for _, gi := range ve.Groups {
			g, err := groupAt(doc.Groups, gi)
			if err != nil {
				return nil, fmt.Errorf("decode image state v2: entry %s: %w", dest, err)
			}
			e.Refs = append(e.Refs, ImageRef(g))
		}
		state[dest] = e
	}
	return state, nil
}

// decode reads a single ConfigMap in any supported format: a v2 document, a
// legacy v1 gzip blob, or the plain-JSON debugging fallback.
func decode(cm *corev1.ConfigMap) (ImageState, error) {
	if data, ok := cm.BinaryData[blobKeyV2]; ok {
		return decodeV2(data)
	}
	state := make(ImageState)
	if gz, ok := cm.BinaryData[blobKeyV1]; ok {
		r, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return nil, fmt.Errorf("decode image state: gzip reader: %w", err)
		}
		defer func() { _ = r.Close() }()
		if err := json.NewDecoder(r).Decode(&state); err != nil {
			return nil, fmt.Errorf("decode image state: json decode: %w", err)
		}
		return state, nil
	}
	// Fallback: plain JSON (written by older versions or for debugging)
	if data, ok := cm.Data[blobKeyV1Plain]; ok {
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			return nil, fmt.Errorf("decode image state: json unmarshal: %w", err)
		}
	}
	return state, nil
}
