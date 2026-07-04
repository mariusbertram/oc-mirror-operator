// Package imagestate manages per-image mirroring state via a ConfigMap,
// avoiding size limits of Kubernetes CR status fields.
//
// Each ImageSet gets one ConfigMap named "<imageset-name>-images" in the same
// namespace. It stores a gzip-compressed JSON map of destination → ImageEntry.
// At ~336 bytes/image (uncompressed) gzip reduces this to ~30 bytes/image,
// handling 50,000+ images without hitting the 1 MiB ConfigMap limit.
package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"

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
// MirrorTarget. This is the single per-MirrorTarget state store that replaces
// the per-ImageSet "<imageset>-images" ConfigMaps.
func ConfigMapNameForTarget(mtName string) string {
	return mtName + "-images"
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

// LoadByConfigMapName reads the ImageState from a ConfigMap with the given name.
// Returns an empty ImageState (not nil) if the ConfigMap does not exist.
func LoadByConfigMapName(ctx context.Context, c client.Client, namespace, cmName string) (ImageState, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return make(ImageState), nil
		}
		return nil, fmt.Errorf("get image state configmap: %w", err)
	}
	return decode(cm)
}

// LoadForTarget reads the consolidated ImageState from the per-MirrorTarget
// ConfigMap. Returns an empty ImageState (not nil) if the ConfigMap does not
// exist yet.
func LoadForTarget(ctx context.Context, c client.Client, namespace, mtName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapNameForTarget(mtName))
}

// SaveForTarget writes the consolidated ImageState to the per-MirrorTarget
// ConfigMap "<mtName>-images".
func SaveForTarget(ctx context.Context, c client.Client, namespace, mtName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	return SaveRaw(ctx, c, namespace, ConfigMapNameForTarget(mtName), state, owner, scheme)
}

// SaveRaw writes the ImageState to a ConfigMap with the given name.
// Used for temporary cleanup state that is not owned by an ImageSet.
// If owner and scheme are provided, a ControllerReference is set on the ConfigMap.
func SaveRaw(ctx context.Context, c client.Client, namespace, cmName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	data, err := encode(state)
	if err != nil {
		return fmt.Errorf("encode image state: %w", err)
	}

	existing := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		BinaryData: map[string][]byte{
			"images.json.gz": data,
		},
	}

	if owner != nil && scheme != nil {
		if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}
	}

	if errors.IsNotFound(getErr) {
		return c.Create(ctx, cm)
	}
	if getErr != nil {
		return getErr
	}
	cm.ResourceVersion = existing.ResourceVersion
	return c.Update(ctx, cm)
}

func encode(state ImageState) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(state); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decode(cm *corev1.ConfigMap) (ImageState, error) {
	state := make(ImageState)
	if gz, ok := cm.BinaryData["images.json.gz"]; ok {
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
	if data, ok := cm.Data["images.json"]; ok {
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			return nil, fmt.Errorf("decode image state: json unmarshal: %w", err)
		}
	}
	return state, nil
}
