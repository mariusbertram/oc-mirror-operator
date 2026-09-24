// Package imagestate manages per-image mirroring state via ConfigMaps,
// avoiding size limits of Kubernetes CR status fields.
//
// Each ImageSet owns one ConfigMap named "<imageset-name>-images" in the same
// namespace, holding only that ImageSet's own images. A separate, small
// per-MirrorTarget ConfigMap ("<mirrortarget-name>-images-index") tracks only
// the images referenced by more than one ImageSet, so cleanup can check
// cross-ImageSet sharing without loading every other ImageSet's state. Both
// are gzip-compressed JSON, handling 50,000+ images per ConfigMap without
// hitting the 1 MiB limit.
package imagestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
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
// entry.
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

// ImageEntry tracks the mirroring state of a single image within one
// ImageSet's own state. The destination image reference is the map key in
// ImageState. Ownership is implicit: an entry only exists in the ImageSets
// whose own ConfigMap contains it. Images referenced by more than one
// ImageSet are additionally recorded in the SharedIndex (see IndexConfigMapName).
type ImageEntry struct {
	Source     string `json:"source"`
	State      string `json:"state"` // Pending | Mirrored | Failed
	LastError  string `json:"lastError,omitempty"`
	RetryCount int    `json:"retryCount,omitempty"`
	// Origin records which collector produced this entry.
	Origin ImageOrigin `json:"origin,omitempty"`
	// EntrySig is the per-spec-entry signature that produced this entry.
	EntrySig string `json:"entrySig,omitempty"`
	// OriginRef is a human-readable label describing which spec entry produced
	// this entry. Display only — never parse data out of it (see Catalog).
	OriginRef string `json:"originRef,omitempty"`
	// Catalog is the source operator catalog reference this entry was
	// resolved from. Only set for Origin == OriginOperator entries; entries
	// written before the field existed get it back-filled from OriginRef
	// (see CatalogFromOriginRef).
	Catalog string `json:"catalog,omitempty"`
	// IsBundleImage is true when this entry's image is itself an operator
	// bundle's own container image (as opposed to one of its related/operand
	// images). Only set for Origin == OriginOperator entries. Consulted by
	// the manager to dispatch bundle images ahead of related images, since
	// bundle images are what OLM installs/upgrades to directly.
	IsBundleImage bool `json:"isBundleImage,omitempty"`
	// PermanentlyFailed is set to true when the image has exhausted its initial
	// retry budget (RetryCount >= MirrorTarget.spec.maxRetries, default 10). Once set it is never cleared — even when
	// the image is reset to Pending for a drift-check retry attempt. This flag
	// is used to keep the catalog-build gate open and to surface the image in
	// failedImageDetails regardless of the current retry state.
	PermanentlyFailed bool `json:"permanentlyFailed,omitempty"`
	// NextRetryAt is the earliest time a Failed entry is retried (exponential
	// backoff by RetryCount, see the manager's retryBackoff). Nil means
	// "retry right away"; cleared whenever the entry leaves the Failed state.
	NextRetryAt *metav1.Time `json:"nextRetryAt,omitempty"`
	// SignatureVerified is set once the manager's drift check has confirmed a
	// cosign signature exists for this entry's destination, for ImageSets
	// with Mirror.RequireSignedImages set. Not implied by State == Mirrored:
	// an entry can be Mirrored before the requirement existed or before the
	// check has run, in which case this stays false until the next check.
	SignatureVerified bool `json:"signatureVerified,omitempty"`
	// SourceDigest is the upstream manifest digest last observed for a
	// tag-referenced source (currently only populated for OriginAdditional
	// entries; see MirrorManager.handleStatusUpdate and
	// MirrorManager.additionalImageDriftedLocked). Empty for digest-pinned
	// sources, which cannot drift. Used to detect when a mutable tag has
	// moved upstream so the entry can be reset to Pending instead of staying
	// Mirrored against stale content forever.
	SourceDigest string `json:"sourceDigest,omitempty"`
}

// CatalogFromOriginRef recovers the source catalog from an operator entry's
// OriginRef label ("<catalog> [pkg1, pkg2]" or "<catalog> [..] — <bundle>"),
// for entries written before ImageEntry.Catalog existed.
func CatalogFromOriginRef(originRef string) string {
	catalog, _, _ := strings.Cut(originRef, " ")
	return catalog
}

// ImageState maps destination image reference → ImageEntry, scoped to a
// single ImageSet.
type ImageState map[string]*ImageEntry

// SharedIndex maps destination image reference → the names of the ImageSets
// that reference it. It only ever holds entries for images referenced by two
// or more ImageSets — an image needed by exactly one ImageSet is tracked
// solely in that ImageSet's own ImageState and never appears here. This keeps
// the index small regardless of how large any individual ImageSet's catalogs
// are, since most images are exclusive to the ImageSet that resolved them.
type SharedIndex map[string][]string

// AddSharedRef records that imageSet references dest, appending it if not
// already present. Callers should only call this once dest is known to be
// referenced by a second ImageSet — the index does not track single-owner
// destinations.
func (idx SharedIndex) AddSharedRef(dest, imageSet string) {
	for _, n := range idx[dest] {
		if n == imageSet {
			return
		}
	}
	idx[dest] = append(idx[dest], imageSet)
}

// RemoveSharedRef removes imageSet from dest's entry. If at most one name
// remains, the entry is deleted entirely — a destination referenced by 0 or 1
// ImageSets is not "shared" and has no place in the index.
func (idx SharedIndex) RemoveSharedRef(dest, imageSet string) {
	names := idx[dest]
	out := names[:0]
	for _, n := range names {
		if n != imageSet {
			out = append(out, n)
		}
	}
	if len(out) <= 1 {
		delete(idx, dest)
		return
	}
	idx[dest] = out
}

// IsShared reports whether dest is referenced by more than one ImageSet.
func (idx SharedIndex) IsShared(dest string) bool {
	return len(idx[dest]) >= 2
}

// Names returns the ImageSet names referencing dest, or nil if dest is not
// shared (or unknown).
func (idx SharedIndex) Names(dest string) []string {
	return idx[dest]
}

// ConfigMapName returns the ConfigMap name owned by a single ImageSet.
func ConfigMapName(imageSetName string) string {
	return imageSetName + "-images"
}

// IndexConfigMapName returns the shared-image index ConfigMap name for a
// MirrorTarget.
func IndexConfigMapName(mtName string) string {
	return mtName + "-images-index"
}

// OrphansConfigMapName returns the pending-orphans ConfigMap name for a
// MirrorTarget. The manager appends destinations here when an ImageSet's
// resolve/merge step drops an image that was exclusive to it (spec narrowing
// or blocking); the MirrorTarget controller consumes it to create a cleanup
// Job, the same way it does for a fully removed ImageSet.
func OrphansConfigMapName(mtName string) string {
	return mtName + "-images-orphans"
}

// Deprecated: ConfigMapNameForTarget returns the legacy consolidated
// per-MirrorTarget ConfigMap name. Used only by MigrateConsolidatedToPerImageSet
// to locate and migrate away from the pre-partitioning store.
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

// Load reads the ImageState owned by a single ImageSet. Returns an empty
// ImageState (not nil) if the ConfigMap does not exist yet.
func Load(ctx context.Context, c client.Client, namespace, imageSetName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapName(imageSetName))
}

// Save writes the ImageState to the given ImageSet's own ConfigMap
// ("<imageSetName>-images").
func Save(ctx context.Context, c client.Client, namespace, imageSetName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	return SaveRaw(ctx, c, namespace, ConfigMapName(imageSetName), state, owner, scheme)
}

// CatalogDigestsAnnotation is set on an ImageSet's own state ConfigMap and
// holds (as JSON) the operator-catalog cache annotations — key
// mirrorv1alpha1.CatalogDigestAnnotationKey(sig), value the resolved catalog
// digest token — that the entries in the same ConfigMap were resolved from.
// Because both are written in a single ConfigMap update, a reader can never
// observe a catalog digest together with image entries that belong to a
// different (older or newer) resolution of that catalog.
const CatalogDigestsAnnotation = "mirror.openshift.io/resolved-catalog-digests"

// FormatAnnotation marks every image-state ConfigMap written by this package
// with its storage format, so MigrateConsolidatedToPerImageSet can tell a
// current per-ImageSet ConfigMap apart from the legacy consolidated one —
// both are named "<name>-images", and a MirrorTarget may share its name with
// one of its ImageSets.
const FormatAnnotation = "mirror.openshift.io/imagestate-format"

// FormatPerImageSet is the FormatAnnotation value of the current format.
const FormatPerImageSet = "v2"

// SaveWithCatalogDigests is Save plus CatalogDigestsAnnotation, written in
// the same ConfigMap update as the entries.
func SaveWithCatalogDigests(ctx context.Context, c client.Client, namespace, imageSetName string, state ImageState, digests map[string]string, owner metav1.Object, scheme *runtime.Scheme) error {
	var annotations map[string]string
	if len(digests) > 0 {
		raw, err := json.Marshal(digests)
		if err != nil {
			return fmt.Errorf("encode catalog digests: %w", err)
		}
		annotations = map[string]string{CatalogDigestsAnnotation: string(raw)}
	}
	return saveRaw(ctx, c, namespace, ConfigMapName(imageSetName), state, annotations, owner, scheme)
}

// LoadWithCatalogDigests is Load plus the CatalogDigestsAnnotation read from
// the same ConfigMap object, so entries and digests always come from one
// consistent write. Both are empty (not nil) when the ConfigMap is missing.
func LoadWithCatalogDigests(ctx context.Context, c client.Client, namespace, imageSetName string) (ImageState, map[string]string, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ConfigMapName(imageSetName)}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return make(ImageState), map[string]string{}, nil
		}
		return nil, nil, fmt.Errorf("get image state configmap: %w", err)
	}
	state, err := decode(cm)
	if err != nil {
		return nil, nil, err
	}
	digests := map[string]string{}
	if raw := cm.Annotations[CatalogDigestsAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &digests); err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", CatalogDigestsAnnotation, err)
		}
	}
	return state, digests, nil
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

// Deprecated: LoadForTarget reads the legacy consolidated ImageState for a
// MirrorTarget. Used only by MigrateConsolidatedToPerImageSet.
func LoadForTarget(ctx context.Context, c client.Client, namespace, mtName string) (ImageState, error) {
	return LoadByConfigMapName(ctx, c, namespace, ConfigMapNameForTarget(mtName))
}

// SaveRaw writes the ImageState to a ConfigMap with the given name.
// Used for cleanup/orphan snapshot state that is not owned by an ImageSet.
// If owner and scheme are provided, a ControllerReference is set on the ConfigMap.
func SaveRaw(ctx context.Context, c client.Client, namespace, cmName string, state ImageState, owner metav1.Object, scheme *runtime.Scheme) error {
	return saveRaw(ctx, c, namespace, cmName, state, nil, owner, scheme)
}

func saveRaw(ctx context.Context, c client.Client, namespace, cmName string, state ImageState, annotations map[string]string, owner metav1.Object, scheme *runtime.Scheme) error {
	data, err := encode(state)
	if err != nil {
		return fmt.Errorf("encode image state: %w", err)
	}

	existing := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)

	allAnnotations := map[string]string{FormatAnnotation: FormatPerImageSet}
	for k, v := range annotations {
		allAnnotations[k] = v
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cmName,
			Namespace:   namespace,
			Annotations: allAnnotations,
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

// LoadIndex reads the shared-image index for a MirrorTarget. Returns an empty
// (not nil) SharedIndex if the ConfigMap does not exist yet.
func LoadIndex(ctx context.Context, c client.Client, namespace, mtName string) (SharedIndex, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: IndexConfigMapName(mtName)}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return make(SharedIndex), nil
		}
		return nil, fmt.Errorf("get shared image index configmap: %w", err)
	}
	return decodeIndex(cm)
}

// SaveIndex writes the shared-image index for a MirrorTarget. An empty index
// deletes the ConfigMap (if present) rather than persisting an empty object,
// since the common case — no shared images — should leave no trace.
func SaveIndex(ctx context.Context, c client.Client, namespace, mtName string, idx SharedIndex, owner metav1.Object, scheme *runtime.Scheme) error {
	cmName := IndexConfigMapName(mtName)
	if len(idx) == 0 {
		err := c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: namespace}})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete empty shared image index configmap: %w", err)
		}
		return nil
	}

	data, err := encodeIndex(idx)
	if err != nil {
		return fmt.Errorf("encode shared image index: %w", err)
	}

	existing := &corev1.ConfigMap{}
	getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		BinaryData: map[string][]byte{
			"index.json.gz": data,
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
	return encodeGzipJSON(state)
}

func decode(cm *corev1.ConfigMap) (ImageState, error) {
	state := make(ImageState)
	if err := decodeGzipJSON(cm, "images.json.gz", "images.json", &state); err != nil {
		return nil, err
	}
	return state, nil
}

func encodeIndex(idx SharedIndex) ([]byte, error) {
	return encodeGzipJSON(idx)
}

func decodeIndex(cm *corev1.ConfigMap) (SharedIndex, error) {
	idx := make(SharedIndex)
	if err := decodeGzipJSON(cm, "index.json.gz", "index.json", &idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func encodeGzipJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(v); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeGzipJSON decodes cm's BinaryData[gzKey] (gzip+JSON) into out, falling
// back to cm.Data[plainKey] (plain JSON, written by older versions or for
// debugging) when the binary key is absent. out must be a pointer to a map
// type; an empty/missing ConfigMap decodes to an unmodified (already
// zero-value) *out.
func decodeGzipJSON(cm *corev1.ConfigMap, gzKey, plainKey string, out any) error {
	if gz, ok := cm.BinaryData[gzKey]; ok {
		r, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return fmt.Errorf("decode %s: gzip reader: %w", gzKey, err)
		}
		defer func() { _ = r.Close() }()
		if err := json.NewDecoder(r).Decode(out); err != nil {
			return fmt.Errorf("decode %s: json decode: %w", gzKey, err)
		}
		return nil
	}
	if data, ok := cm.Data[plainKey]; ok {
		if err := json.Unmarshal([]byte(data), out); err != nil {
			return fmt.Errorf("decode %s: json unmarshal: %w", plainKey, err)
		}
	}
	return nil
}

// legacyImageRef mirrors the pre-partitioning per-ImageSet ownership record.
// Used only to decode the legacy consolidated ConfigMap during migration.
type legacyImageRef struct {
	ImageSet  string      `json:"imageSet"`
	Origin    ImageOrigin `json:"origin,omitempty"`
	EntrySig  string      `json:"entrySig,omitempty"`
	OriginRef string      `json:"originRef,omitempty"`
}

// legacyImageEntry mirrors the pre-partitioning consolidated ImageEntry shape
// (with Refs). Used only by MigrateConsolidatedToPerImageSet.
type legacyImageEntry struct {
	Source            string           `json:"source"`
	State             string           `json:"state"`
	LastError         string           `json:"lastError,omitempty"`
	RetryCount        int              `json:"retryCount,omitempty"`
	PermanentlyFailed bool             `json:"permanentlyFailed,omitempty"`
	Origin            ImageOrigin      `json:"origin,omitempty"`
	EntrySig          string           `json:"entrySig,omitempty"`
	OriginRef         string           `json:"originRef,omitempty"`
	Refs              []legacyImageRef `json:"refs,omitempty"`
}

// MigrateConsolidatedToPerImageSet reads the legacy consolidated
// per-MirrorTarget ConfigMap ("<mt>-images"), if present, and splits it into
// one ConfigMap per referencing ImageSet plus the shared-image index, then
// deletes the consolidated ConfigMap. It is a no-op if the consolidated
// ConfigMap does not exist (already migrated, or a fresh install that never
// had one).
//
// Entries with no Refs and no flat Origin (orphans awaiting cleanup under the
// pre-partitioning model) are dropped rather than migrated — they carry no
// image an ImageSet still needs and would otherwise never be cleaned up under
// the new model, which has no consolidated map left to scan for them.
//
// "<mt>-images" is also the name of the per-ImageSet ConfigMap of an ImageSet
// named like its MirrorTarget, so the ConfigMap is only treated as legacy if
// it carries no FormatAnnotation and does not look like the current format
// (entries without Refs but with a flat Origin, as written before the
// annotation existed). Otherwise it is left alone: deleting it would wipe
// that ImageSet's state on every manager start.
func MigrateConsolidatedToPerImageSet(ctx context.Context, c client.Client, namespace, mtName string, owner metav1.Object, scheme *runtime.Scheme) error {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ConfigMapNameForTarget(mtName)}, cm)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get legacy consolidated image state configmap: %w", err)
	}

	if cm.Annotations[FormatAnnotation] != "" {
		return nil
	}

	consolidated := make(map[string]*legacyImageEntry)
	if decErr := decodeGzipJSON(cm, "images.json.gz", "images.json", &consolidated); decErr != nil {
		return fmt.Errorf("decode legacy consolidated image state: %w", decErr)
	}
	if !looksLegacy(consolidated) {
		return nil
	}

	perImageSet := make(map[string]ImageState)
	index := make(SharedIndex)
	for dest, entry := range consolidated {
		if entry == nil || len(entry.Refs) == 0 {
			continue
		}
		if len(entry.Refs) > 1 {
			names := make([]string, 0, len(entry.Refs))
			for _, ref := range entry.Refs {
				names = append(names, ref.ImageSet)
			}
			index[dest] = names
		}
		for _, ref := range entry.Refs {
			state, ok := perImageSet[ref.ImageSet]
			if !ok {
				state = make(ImageState)
				perImageSet[ref.ImageSet] = state
			}
			state[dest] = &ImageEntry{
				Source:            entry.Source,
				State:             entry.State,
				LastError:         entry.LastError,
				RetryCount:        entry.RetryCount,
				PermanentlyFailed: entry.PermanentlyFailed,
				Origin:            ref.Origin,
				EntrySig:          ref.EntrySig,
				OriginRef:         ref.OriginRef,
			}
		}
	}

	for isName, state := range perImageSet {
		if saveErr := Save(ctx, c, namespace, isName, state, owner, scheme); saveErr != nil {
			return fmt.Errorf("migrate state for imageset %s: %w", isName, saveErr)
		}
	}
	if len(index) > 0 {
		if saveErr := SaveIndex(ctx, c, namespace, mtName, index, owner, scheme); saveErr != nil {
			return fmt.Errorf("migrate shared image index: %w", saveErr)
		}
	}

	// An ImageSet named like the MirrorTarget was just migrated into this
	// very ConfigMap — it now holds that ImageSet's state and must stay.
	if _, reused := perImageSet[mtName]; reused {
		return nil
	}
	if delErr := c.Delete(ctx, cm); delErr != nil && !errors.IsNotFound(delErr) {
		return fmt.Errorf("delete legacy consolidated image state configmap: %w", delErr)
	}
	return nil
}

// looksLegacy reports whether a decoded, unannotated "<name>-images" map is
// the legacy consolidated format: any entry with Refs is legacy; entries
// without Refs but with a flat Origin are the current per-ImageSet format; a
// map of only Ref-less, Origin-less entries (legacy orphans) or an empty map
// is legacy.
func looksLegacy(entries map[string]*legacyImageEntry) bool {
	current := false
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if len(entry.Refs) > 0 {
			return true
		}
		if entry.Origin != "" {
			current = true
		}
	}
	return !current
}
