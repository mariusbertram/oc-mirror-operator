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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
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
)

// ImageRef tracks metadata for an image reference relative to a specific ImageSet.
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
	// Deprecated: use Refs[].Origin instead
	Origin ImageOrigin `json:"origin,omitempty"`
	// EntrySig is the per-spec-entry signature that produced this entry —
	// e.g. the OperatorEntrySignature for OriginOperator or the
	// ReleaseChannelSignature for OriginRelease. It allows the manager to
	// carry forward only entries belonging to a cache-hit spec entry while
	// dropping entries from removed/changed entries.
	// Empty for OriginAdditional and for legacy entries; partition logic
	// must treat empty as "any sig" to remain backward-compatible.
	// Deprecated: use Refs[].EntrySig instead
	EntrySig string `json:"entrySig,omitempty"`
	// OriginRef is a human-readable label describing which spec entry produced
	// this entry, e.g. "registry.../redhat-operator-index:v4.21 [web-terminal]"
	// or "stable-4.14 [amd64]". Used to surface failed-image details in status.
	// Deprecated: use Refs[].OriginRef instead
	OriginRef string `json:"originRef,omitempty"`
	// PermanentlyFailed is set to true when the image has exhausted its initial
	// retry budget (RetryCount >= 10). Once set it is never cleared — even when
	// the image is reset to Pending for a drift-check retry attempt. This flag
	// is used to keep the catalog-build gate open and to surface the image in
	// failedImageDetails regardless of the current retry state.
	PermanentlyFailed bool `json:"permanentlyFailed,omitempty"`
	// Refs tracks which ImageSets reference this image.
	Refs []ImageRef `json:"refs,omitempty"`
}

// HasImageSet returns true if the entry is referenced by the given ImageSet.
func (e *ImageEntry) HasImageSet(name string) bool {
	for _, ref := range e.Refs {
		if ref.ImageSet == name {
			return true
		}
	}
	return false
}

// AddRef adds or updates a reference for the given ImageSet.
func (e *ImageEntry) AddRef(ref ImageRef) {
	for i, existing := range e.Refs {
		if existing.ImageSet == ref.ImageSet {
			e.Refs[i] = ref
			return
		}
	}
	e.Refs = append(e.Refs, ref)
}

// RemoveImageSet removes the reference for the given ImageSet.
// Returns true if the reference was found and removed.
func (e *ImageEntry) RemoveImageSet(name string) bool {
	for i, ref := range e.Refs {
		if ref.ImageSet == name {
			e.Refs = append(e.Refs[:i], e.Refs[i+1:]...)
			return true
		}
	}
	return false
}

// ImageSetNames returns a slice of ImageSet names referencing this image.
func (e *ImageEntry) ImageSetNames() []string {
	names := make([]string, len(e.Refs))
	for i, ref := range e.Refs {
		names[i] = ref.ImageSet
	}
	return names
}

// ImageState maps destination image reference → ImageEntry.
type ImageState map[string]*ImageEntry

// ConfigMapName returns the ConfigMap name for a given ImageSet.
func ConfigMapName(imageSetName string) string {
	return imageSetName + "-images"
}

// ConfigMapNameForTarget returns the ConfigMap name for a given MirrorTarget.
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

// CountsForImageSet returns aggregate counts for images referenced by the given ImageSet.
func CountsForImageSet(state ImageState, isName string) (total, mirrored, pending, failed int) {
	for _, e := range state {
		if !e.HasImageSet(isName) {
			continue
		}
		total++
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

// Load reads the ImageState from the ConfigMap for the given ImageSet.
// Returns an empty ImageState (not nil) if the ConfigMap does not exist.
func Load(ctx context.Context, c client.Client, namespace, imageSetName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapName(imageSetName))
}

// LoadWithExistence reads the ImageState and reports whether the ConfigMap
// actually exists in the cluster. This allows callers to distinguish "ConfigMap
// was deleted" from "ConfigMap exists but is empty".
func LoadWithExistence(ctx context.Context, c client.Client, namespace, imageSetName string) (state ImageState, exists bool, err error) {
	cm := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ConfigMapName(imageSetName)}, cm)
	if getErr != nil {
		if errors.IsNotFound(getErr) {
			return make(ImageState), false, nil
		}
		return nil, false, fmt.Errorf("get image state configmap: %w", getErr)
	}
	s, decodeErr := decode(cm)
	if decodeErr != nil {
		return nil, true, decodeErr
	}
	return s, true, nil
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

// Save writes the ImageState to the ConfigMap for the given ImageSet.
func Save(ctx context.Context, c client.Client, namespace string, is *mirrorv1alpha1.ImageSet, state ImageState) error {
	owners := []metav1.OwnerReference{{
		APIVersion:         mirrorv1alpha1.GroupVersion.String(),
		Kind:               "ImageSet",
		Name:               is.Name,
		UID:                is.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(false),
	}}
	return saveInternal(ctx, c, namespace, ConfigMapName(is.Name), owners, state)
}

// SaveRaw writes the ImageState to a ConfigMap with the given name.
func SaveRaw(ctx context.Context, c client.Client, namespace, cmName string, state ImageState) error {
	return saveInternal(ctx, c, namespace, cmName, nil, state)
}

// SaveForTarget writes the ImageState to the ConfigMap for the given MirrorTarget.
func SaveForTarget(ctx context.Context, c client.Client, namespace string, mt *mirrorv1alpha1.MirrorTarget, state ImageState) error {
	owners := []metav1.OwnerReference{{
		APIVersion:         mirrorv1alpha1.GroupVersion.String(),
		Kind:               "MirrorTarget",
		Name:               mt.Name,
		UID:                mt.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(false),
	}}
	return saveInternal(ctx, c, namespace, ConfigMapNameForTarget(mt.Name), owners, state)
}

// LoadForTarget reads the ImageState from the ConfigMap for the given MirrorTarget.
// Returns an empty ImageState (not nil) if the ConfigMap does not exist.
func LoadForTarget(ctx context.Context, c client.Client, namespace, mtName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapNameForTarget(mtName))
}

func saveInternal(ctx context.Context, c client.Client, namespace, cmName string, owners []metav1.OwnerReference, state ImageState) error {
	data, err := encode(state)
	if err != nil {
		return fmt.Errorf("encode image state: %w", err)
	}

	existing := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cmName,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		BinaryData: map[string][]byte{
			"images.json.gz": data,
		},
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

func ptr[T any](v T) *T { return &v }
