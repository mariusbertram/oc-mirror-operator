/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
	pkgrelease "github.com/mariusbertram/oc-mirror-operator/pkg/release"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// operatorCacheVersion is bumped whenever the operator resolution or filtering
// logic changes semantically (e.g. heads-only channel filtering).  Old cached
// annotation values that were written with a different (or no) version prefix
// will not match the fresh value, forcing a re-resolution.
const operatorCacheVersion = "v4"

// operatorCacheValue builds the cache token written to the ImageSet annotation.
func operatorCacheValue(digest string) string {
	return operatorCacheVersion + ":" + digest
}

func (m *MirrorManager) saveCatalogPackages(ctx context.Context, slug string, info resources.CatalogInfo, cfg *declcfg.DeclarativeConfig) error {
	resp := resources.BuildCatalogPackagesResponse(info, cfg)
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal catalog packages: %w", err)
	}

	cmName := fmt.Sprintf("oc-mirror-%s-packages", slug)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"oc-mirror.openshift.io/catalog-packages": slug,
			},
		},
		Data: map[string]string{
			"packages.json": string(data),
		},
	}

	// Use CreateOrUpdate for idempotency
	existing := &corev1.ConfigMap{}
	err = m.Client.Get(ctx, client.ObjectKey{Name: cmName, Namespace: m.Namespace}, existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return m.Client.Create(ctx, cm)
	}

	existing.Data = cm.Data
	existing.Labels = cm.Labels
	return m.Client.Update(ctx, existing)
}

// operatorCacheHit returns true if the cached annotation value matches the
// current cache token for the given digest.
func operatorCacheHit(cached, digest string) bool {
	return cached != "" && cached == operatorCacheValue(digest)
}

// resolveImageSet enumerates the upstream content (releases, operator
// catalogs, additional images) referenced by an ImageSet, merges the result
// into the per-ImageSet imagestate ConfigMap, and refreshes the digest cache
// annotations.
//
// Caching: each spec entry has a stable signature (sig) computed via
// api/v1alpha1.OperatorEntrySignature / ReleaseChannelSignature. The ImageSet
// annotations "mirror.openshift.io/{catalog,release}-digest-<sig>" store the
// last-resolved upstream digest (catalog) or resolved-payload-list signature
// (release). On a cache hit the existing imagestate entries with matching
// (Origin, EntrySig) are carried over without doing the expensive component
// extraction or FBC parse.
//
// Partitioning: every entry written by this method records its EntrySig in
// the ImageEntry. carry-over copies only entries whose EntrySig matches the
// hit entry, so a cache-hit on op-A does NOT pull in stale entries that came
// from op-B in a previous resolution.
//
// recollect annotation: "mirror.openshift.io/recollect" forces a full bypass
// of all cache hits for this round; it is removed by this method as a
// one-shot trigger.
//
// Concurrency: the caller releases the manager mutex around resolveImageSet
// because cheap-but-non-zero network I/O happens here. State is merged with
// the live in-memory imagestate after re-acquiring the lock; see
// reconcile() in manager.go for the merge path that preserves concurrent
// worker callbacks.
func (m *MirrorManager) resolveImageSet(ctx context.Context, is *mirrorv1alpha1.ImageSet, mt *mirrorv1alpha1.MirrorTarget, currentState imagestate.ImageState) (imagestate.ImageState, bool, error) {
	if currentState == nil {
		currentState = make(imagestate.ImageState)
	}

	collector, resolver := m.buildCollector(mt)

	annotations := is.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	_, recollect := annotations[mirrorv1alpha1.RecollectAnnotation]
	annotationsChanged := false
	newAnnotations := copyMap(annotations)

	newState := make(imagestate.ImageState, len(currentState))
	// Pre-populate with entries this method does NOT own (legacy / unknown
	// origins) so we don't accidentally drop them.
	for dest, entry := range currentState {
		if entry == nil {
			continue
		}
		switch entry.Origin {
		case imagestate.OriginRelease, imagestate.OriginOperator, imagestate.OriginAdditional:
			// owned — handled below
		default:
			cp := *entry
			newState[dest] = &cp
		}
	}

	releaseChanged, err := m.resolveReleaseSection(ctx, collector, is, mt, currentState, newState, newAnnotations, recollect)
	if err != nil {
		return nil, false, fmt.Errorf("resolve releases: %w", err)
	}
	if releaseChanged {
		annotationsChanged = true
	}

	operatorChanged, err := m.resolveOperatorSection(ctx, collector, resolver, is, mt, currentState, newState, newAnnotations, recollect)
	if err != nil {
		return nil, false, fmt.Errorf("resolve operators: %w", err)
	}
	if operatorChanged {
		annotationsChanged = true
	}

	// Additional images are cheap to enumerate; always re-collected.
	additional, err := collector.CollectAdditional(ctx, &is.Spec, mt, nil)
	if err != nil {
		return nil, false, fmt.Errorf("collect additional images: %w", err)
	}
	mergeIntoStateWithSig(newState, additional, imagestate.OriginAdditional, "", "additional", currentState)

	// Drop any annotation whose sig is no longer in spec.
	if pruneObsoleteCacheAnnotations(newAnnotations, is) {
		annotationsChanged = true
	}

	// Clear the recollect annotation if it was honored.
	if recollect {
		delete(newAnnotations, mirrorv1alpha1.RecollectAnnotation)
		annotationsChanged = true
	}

	if annotationsChanged {
		if err := m.patchImageSetAnnotations(ctx, is, newAnnotations); err != nil {
			fmt.Printf("Warning: failed to patch annotations on ImageSet %s: %v\n", is.Name, err)
		}
	}

	stateChanged := !equalState(currentState, newState)
	return newState, stateChanged, nil
}

// buildCollector returns a Collector + CatalogResolver pair using
// MirrorTarget-aware client (insecure-host config when set).
func (m *MirrorManager) buildCollector(mt *mirrorv1alpha1.MirrorTarget) (*mirror.Collector, *catalog.CatalogResolver) {
	mc := m.mirrorClient
	if mt.Spec.Insecure {
		host := mt.Spec.Registry
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		mc = mirrorclient.NewMirrorClient([]string{host}, m.authConfigPath)
	}
	return mirror.NewCollector(mc), catalog.New(mc)
}

func (m *MirrorManager) resolveReleaseSection( //nolint:unparam
	ctx context.Context,
	collector *mirror.Collector,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	currentState imagestate.ImageState,
	newState imagestate.ImageState,
	annotations map[string]string,
	recollect bool,
) (bool, error) { //nolint:unparam
	annoChanged := false
	arch := is.Spec.Mirror.Platform.Architectures
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}

	for _, ch := range is.Spec.Mirror.Platform.Channels {
		sig := mirrorv1alpha1.ReleaseChannelSignature(ch, arch, is.Spec.Mirror.Platform.KubeVirtContainer)
		annoKey := mirrorv1alpha1.ReleaseDigestAnnotationKey(sig)
		cached := annotations[annoKey]
		originRef := fmt.Sprintf("%s [%s]", ch.Name, strings.Join(arch, ","))

		payloadNodes, resolveErr := collector.ResolveReleasePayloadNodes(ctx, ch, arch)
		if resolveErr != nil {
			fmt.Printf("Warning: probe release channel %s: %v\n", ch.Name, resolveErr)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			continue
		}
		freshSig := release.ResolvedSignature(release.NodeImages(payloadNodes))
		if !recollect && cached != "" && cached == freshSig {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			continue
		}

		images, err := collector.CollectReleasesForChannel(ctx, &is.Spec, mt, ch, payloadNodes)
		if err != nil {
			fmt.Printf("Warning: collect release channel %s: %v\n", ch.Name, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			continue
		}
		mergeIntoStateWithSig(newState, images, imagestate.OriginRelease, sig, originRef, currentState)

		// Download and persist GPG signatures for the resolved release nodes.
		// Failures are best-effort — they do not block the main mirroring flow.
		m.downloadSignaturesForNodes(ctx, payloadNodes)

		if annotations[annoKey] != freshSig {
			annotations[annoKey] = freshSig
			annoChanged = true
		}
	}
	return annoChanged, nil
}

// signatureConfigMapName returns the name of the ConfigMap that stores release
// GPG signatures for this MirrorTarget.
func (m *MirrorManager) signatureConfigMapName() string {
	return m.TargetName + "-signatures"
}

// downloadSignaturesForNodes downloads the GPG signature for each release node
// and persists them in the <mt>-signatures ConfigMap (BinaryData: sha256-<hash>
// → raw GPG bytes). Missing or failed signatures are logged and skipped.
func (m *MirrorManager) downloadSignaturesForNodes(ctx context.Context, nodes []release.Node) {
	if len(nodes) == 0 {
		return
	}
	sigClient := pkgrelease.NewSignatureClient(nil)

	// Load existing signatures so we can skip already-downloaded ones.
	cmName := m.signatureConfigMapName()
	existing := &corev1.ConfigMap{}
	_ = m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: cmName}, existing)
	if existing.BinaryData == nil {
		existing.BinaryData = map[string][]byte{}
	}

	newSigs := map[string][]byte{}
	for _, node := range nodes {
		digest := extractDigest(node.Image)
		if digest == "" {
			continue
		}
		key := strings.ReplaceAll(digest, ":", "-")
		if _, already := existing.BinaryData[key]; already {
			continue
		}
		data, err := sigClient.DownloadSignature(ctx, digest)
		if err != nil {
			fmt.Printf("Warning: download signature for %s: %v\n", digest, err)
			continue
		}
		newSigs[key] = data
	}

	if len(newSigs) == 0 {
		return
	}

	// Merge new signatures into the ConfigMap.
	for k, v := range newSigs {
		existing.BinaryData[k] = v
	}

	if existing.Name == "" {
		existing.Name = cmName
		existing.Namespace = m.Namespace
		existing.ObjectMeta = metav1.ObjectMeta{Name: cmName, Namespace: m.Namespace}
		if err := m.Client.Create(ctx, existing); err != nil && !apierrors.IsAlreadyExists(err) {
			fmt.Printf("Warning: create signatures ConfigMap: %v\n", err)
		}
		return
	}
	if err := m.Client.Update(ctx, existing); err != nil {
		fmt.Printf("Warning: update signatures ConfigMap: %v\n", err)
	}
}

// extractDigest extracts the sha256 digest from an image reference of the form
// "registry/repo@sha256:<hex>" or "registry/repo:tag@sha256:<hex>".
// Returns "" when no digest is found.
func extractDigest(imageRef string) string {
	idx := strings.Index(imageRef, "@sha256:")
	if idx < 0 {
		return ""
	}
	return imageRef[idx+1:] // "sha256:<hex>"
}

func (m *MirrorManager) resolveOperatorSection( //nolint:unparam
	ctx context.Context,
	_ *mirror.Collector,
	resolver *catalog.CatalogResolver,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	currentState imagestate.ImageState,
	newState imagestate.ImageState,
	annotations map[string]string,
	recollect bool,
) (bool, error) { //nolint:unparam
	annoChanged := false

	for _, op := range is.Spec.Mirror.Operators {
		sig := mirrorv1alpha1.OperatorEntrySignature(op)
		annoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(sig)
		cached := annotations[annoKey]

		pkgNames := make([]string, 0, len(op.Packages))
		for _, p := range op.Packages {
			pkgNames = append(pkgNames, p.Name)
		}
		sort.Strings(pkgNames)
		originRef := op.Catalog
		if len(pkgNames) > 0 {
			originRef = fmt.Sprintf("%s [%s]", op.Catalog, strings.Join(pkgNames, ", "))
		}

		freshDigest, err := resolver.GetCatalogDigest(ctx, op.Catalog)
		if err != nil {
			fmt.Printf("Warning: probe catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			continue
		}

		cacheToken := operatorCacheValue(freshDigest)
		if !recollect && operatorCacheHit(cached, freshDigest) {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			continue
		}

		images, cfg, err := resolver.ResolveCatalogFull(ctx, op.Catalog, op.Packages)
		if err != nil {
			fmt.Printf("Warning: collect catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			continue
		}

		targetImages := make([]mirror.TargetImage, 0, len(images))
		for img, label := range images {
			targetImages = append(targetImages, mirror.TargetImage{
				Source:      img,
				BundleRef:   label,
				Destination: mirror.ComponentDestination(mt.Spec.Registry, img),
			})
		}
		mergeIntoStateWithSig(newState, targetImages, imagestate.OriginOperator, sig, originRef, currentState)

		// Phase 7a: Persist catalog package information in a ConfigMap for the Resource API.
		catSlug := resources.CatalogSlug(op.Catalog)
		targetImage := resources.CatalogTargetImage(mt.Spec.Registry, op.Catalog)
		catInfo := resources.CatalogInfo{
			SourceCatalog: op.Catalog,
			TargetImage:   targetImage,
		}
		if err := m.saveCatalogPackages(ctx, catSlug, catInfo, cfg); err != nil {
			fmt.Printf("Warning: failed to save catalog packages for %s: %v\n", catSlug, err)
		}

		if annotations[annoKey] != cacheToken {
			annotations[annoKey] = cacheToken
			annoChanged = true
		}
	}
	return annoChanged, nil
}

// mergeIntoStateWithSig writes entries into dst, tagging each with origin+sig.
// When the same destination already exists in prev with the same Origin (any
// EntrySig), prior State / RetryCount / LastError are preserved so
// already-mirrored images stay mirrored.
//
// When two different sigs produce the same destination (e.g. two operator
// entries that both depend on a shared bundle), the LAST writer wins for
// EntrySig; the State is preserved across either writer because we look up
// `prev` by destination only.
func mergeIntoStateWithSig(dst imagestate.ImageState, images []mirror.TargetImage, origin imagestate.ImageOrigin, sig, originRef string, prev imagestate.ImageState) {
	for _, img := range images {
		// Per-image bundle reference takes precedence over the spec-level
		// catalog+packages label; fall back to originRef when not set.
		ref := originRef
		if img.BundleRef != "" {
			ref = fmt.Sprintf("%s — %s", originRef, img.BundleRef)
		}
		entry := &imagestate.ImageEntry{
			Source:    img.Source,
			State:     "Pending",
			Origin:    origin,
			EntrySig:  sig,
			OriginRef: ref,
		}
		if existing, ok := prev[img.Destination]; ok && existing != nil && existing.Origin == origin {
			// Prefer the existing Source if it looks like a valid reference
			// (i.e. doesn't contain a comma) and the new one might be a bundle list.
			if strings.Contains(entry.Source, ",") && !strings.Contains(existing.Source, ",") {
				entry.Source = existing.Source
			}

			// Only carry forward Mirrored state — work we've already done.
			// Failed entries (including permanently-failed ones) are reset to
			// Pending so they get a fresh attempt whenever the spec changes
			// or recollect is triggered. Cache-hits bypass this function and
			// go through carryOverByOriginAndSig which preserves all states.
			if existing.State == "Mirrored" {
				entry.State = existing.State
				entry.RetryCount = existing.RetryCount
				entry.LastError = existing.LastError
			}
		}
		dst[img.Destination] = entry
	}
}

// carryOverByOriginAndSig copies entries from src into dst that match
// (origin, sig) AND that don't already exist in dst (last writer wins).
//
// originRef is the current spec-level origin label; it is used to back-fill
// any entry whose OriginRef is still empty (written by older controller
// versions before per-image bundle enrichment was introduced).
//
// Backward-compat: entries with empty EntrySig are treated as legacy and
// carried over for any sig matching their Origin so that older state is not
// dropped on first migration. They will be re-tagged with a real sig the
// next time their owning spec entry is re-resolved.
func carryOverByOriginAndSig(src, dst imagestate.ImageState, origin imagestate.ImageOrigin, sig, originRef string) {
	for dest, entry := range src {
		if entry == nil || entry.Origin != origin {
			continue
		}
		if entry.EntrySig != "" && entry.EntrySig != sig {
			continue
		}
		if _, exists := dst[dest]; exists {
			continue
		}
		cp := *entry
		// Adopt the current sig for legacy entries so future cache hits work
		// correctly.
		if cp.EntrySig == "" {
			cp.EntrySig = sig
		}
		// Back-fill OriginRef for entries written before per-image bundle refs
		// were introduced so that failedImageDetails.origin is never empty.
		if cp.OriginRef == "" && originRef != "" {
			cp.OriginRef = originRef
		}
		dst[dest] = &cp
	}
}

func pruneObsoleteCacheAnnotations(annotations map[string]string, is *mirrorv1alpha1.ImageSet) bool {
	desired := map[string]bool{}
	arch := is.Spec.Mirror.Platform.Architectures
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}
	for _, ch := range is.Spec.Mirror.Platform.Channels {
		desired[mirrorv1alpha1.ReleaseDigestAnnotationKey(mirrorv1alpha1.ReleaseChannelSignature(ch, arch, is.Spec.Mirror.Platform.KubeVirtContainer))] = true
	}
	for _, op := range is.Spec.Mirror.Operators {
		desired[mirrorv1alpha1.CatalogDigestAnnotationKey(mirrorv1alpha1.OperatorEntrySignature(op))] = true
	}

	removed := false
	for k := range annotations {
		if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) ||
			strings.HasPrefix(k, mirrorv1alpha1.ReleaseDigestAnnotationPrefix) {
			if !desired[k] {
				delete(annotations, k)
				removed = true
			}
		}
	}
	return removed
}

// patchImageSetAnnotations re-applies the manager-owned cache annotations to
// the ImageSet using retry-on-conflict.
func (m *MirrorManager) patchImageSetAnnotations(ctx context.Context, is *mirrorv1alpha1.ImageSet, desired map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &mirrorv1alpha1.ImageSet{}
		if err := m.Client.Get(ctx, client.ObjectKey{Namespace: is.Namespace, Name: is.Name}, fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		for k := range fresh.Annotations {
			if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) ||
				strings.HasPrefix(k, mirrorv1alpha1.ReleaseDigestAnnotationPrefix) ||
				k == mirrorv1alpha1.RecollectAnnotation {
				delete(fresh.Annotations, k)
			}
		}
		for k, v := range desired {
			if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) ||
				strings.HasPrefix(k, mirrorv1alpha1.ReleaseDigestAnnotationPrefix) {
				fresh.Annotations[k] = v
			}
		}
		return m.Client.Update(ctx, fresh)
	})
}

func equalState(a, b imagestate.ImageState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if va == nil || vb == nil {
			if va != vb {
				return false
			}
			continue
		}
		changed := va.Source != vb.Source ||
			va.State != vb.State ||
			va.Origin != vb.Origin ||
			va.EntrySig != vb.EntrySig ||
			va.RetryCount != vb.RetryCount ||
			va.LastError != vb.LastError ||
			va.OriginRef != vb.OriginRef
		if changed {
			return false
		}
	}
	return true
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneImageState(s imagestate.ImageState) imagestate.ImageState {
	out := make(imagestate.ImageState, len(s))
	for k, v := range s {
		if v == nil {
			out[k] = nil
			continue
		}
		entry := *v
		out[k] = &entry
	}
	return out
}

// mergeWorkerUpdates applies any State/RetryCount/LastError changes that
// happened in `live` (the in-memory map mutated by worker callbacks during
// the resolve unlock window) into `resolved` (the freshly-resolved state).
//
// Rules:
//   - Only destinations present in `resolved` are updated. Destinations
//     that no longer exist in `resolved` are dropped (resolution is the
//     authoritative spec view).
//   - For destinations in both, fields where `live` carries newer worker
//     observations override `resolved`'s defaults: if `live` is "Mirrored"
//     or "Failed" we adopt that state + retryCount + lastError. We do not
//     downgrade Mirrored/Failed back to Pending from `live`.
//
// This guarantees worker pod status callbacks that fired during resolution
// are not lost.
func mergeWorkerUpdates(resolved, live imagestate.ImageState) imagestate.ImageState {
	if live == nil {
		return resolved
	}
	for dest, rEntry := range resolved {
		if rEntry == nil {
			continue
		}
		lEntry, ok := live[dest]
		if !ok || lEntry == nil {
			continue
		}
		if lEntry.State == "Mirrored" || lEntry.State == "Failed" {
			rEntry.State = lEntry.State
			rEntry.RetryCount = lEntry.RetryCount
			rEntry.LastError = lEntry.LastError
			rEntry.PermanentlyFailed = lEntry.PermanentlyFailed
		}
	}
	return resolved
}

// shouldResolve gates resolveImageSet calls so we don't hammer upstream
// registries on every 30s reconcile loop. Resolution runs when:
//   - imagestate is empty (initial resolution)
//   - the recollect annotation is set
//   - the spec has changed (Generation > Status.ObservedGeneration)
//   - a cache annotation carries an outdated operatorCacheVersion (operator
//     binary was upgraded and filtering logic changed)
//   - the configured pollInterval has elapsed since LastSuccessfulPollTime
func shouldResolve(is *mirrorv1alpha1.ImageSet, mt *mirrorv1alpha1.MirrorTarget, currentState imagestate.ImageState) bool {
	if len(currentState) == 0 {
		return true
	}
	if is.Annotations != nil {
		if _, ok := is.Annotations[mirrorv1alpha1.RecollectAnnotation]; ok {
			return true
		}
	}
	if is.Generation > is.Status.ObservedGeneration {
		return true
	}
	if hasStaleCacheAnnotations(is) {
		return true
	}
	pollInterval := 24 * time.Hour
	pollingEnabled := true
	if mt.Spec.PollInterval != nil {
		if mt.Spec.PollInterval.Duration <= 0 {
			pollingEnabled = false
		} else {
			pollInterval = mt.Spec.PollInterval.Duration
			if pollInterval < 1*time.Hour {
				pollInterval = 1 * time.Hour
			}
		}
	}
	if !pollingEnabled {
		return false
	}
	if is.Status.LastSuccessfulPollTime == nil {
		return true
	}
	return time.Since(is.Status.LastSuccessfulPollTime.Time) >= pollInterval
}

// hasStaleCacheAnnotations returns true if any catalog-digest cache annotation
// on the ImageSet was written with an older operatorCacheVersion. This forces
// re-resolution after an operator binary upgrade that changed the filtering
// logic (e.g. heads-only), even if the ImageSet spec itself hasn't changed.
func hasStaleCacheAnnotations(is *mirrorv1alpha1.ImageSet) bool {
	if is.Annotations == nil {
		return false
	}
	prefix := operatorCacheVersion + ":"
	for k, v := range is.Annotations {
		if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) {
			if !strings.HasPrefix(v, prefix) {
				return true
			}
		}
	}
	return false
}

// filterByImageSet returns a per-IS view of the consolidated state. Each entry
// in the result has its IS-specific Ref's Origin/EntrySig/OriginRef promoted to
// the flat fields so that resolveImageSet (which works with flat fields) sees the
// correct per-IS metadata. The Refs slice is not included in the returned copies.
//
// Legacy entries (no Refs) are included unchanged — they belonged to a single IS
// before the migration and are treated as belonging to all ISes.
func filterByImageSet(state imagestate.ImageState, isName string) imagestate.ImageState {
	result := make(imagestate.ImageState, len(state)/2)
	for dest, entry := range state {
		if entry == nil {
			continue
		}
		if len(entry.Refs) == 0 {
			// Legacy entry: include as-is (flat Origin/EntrySig/OriginRef already set).
			cp := *entry
			result[dest] = &cp
			continue
		}
		var matchRef *imagestate.ImageRef
		for i := range entry.Refs {
			if entry.Refs[i].ImageSet == isName {
				matchRef = &entry.Refs[i]
				break
			}
		}
		if matchRef == nil {
			continue
		}
		// Promote IS-specific Ref fields to flat fields for resolveImageSet.
		cp := *entry
		cp.Origin = matchRef.Origin
		cp.EntrySig = matchRef.EntrySig
		cp.OriginRef = matchRef.OriginRef
		cp.Refs = nil
		result[dest] = &cp
	}
	return result
}

// mergeResolvedIntoConsolidated integrates a freshly-resolved per-IS state into
// the consolidated MirrorTarget state. It:
//   - Adds or updates the IS Ref on each destination present in perISState.
//   - Removes the IS Ref from destinations no longer in perISState.
//   - Deletes entries from consolidated when all Refs are gone (entry is orphaned).
//
// Global state fields (State, RetryCount, LastError, PermanentlyFailed) are
// preserved for existing entries — only the IS Ref metadata is updated.
// Source is updated if it changed.
func mergeResolvedIntoConsolidated(consolidated imagestate.ImageState, perISState imagestate.ImageState, isName string) {
	// Step 1: Upsert entries that exist in the new per-IS state.
	for dest, newEntry := range perISState {
		if newEntry == nil {
			continue
		}
		ref := imagestate.ImageRef{
			ImageSet:  isName,
			Origin:    newEntry.Origin,
			EntrySig:  newEntry.EntrySig,
			OriginRef: newEntry.OriginRef,
		}
		if existing, ok := consolidated[dest]; ok {
			existing.AddRef(ref)
			if existing.Source != newEntry.Source {
				existing.Source = newEntry.Source
			}
		} else {
			e := *newEntry
			e.Refs = []imagestate.ImageRef{ref}
			// Clear flat fields — they live in Refs now.
			e.Origin = ""
			e.EntrySig = ""
			e.OriginRef = ""
			consolidated[dest] = &e
		}
	}

	// Step 2: Remove stale IS refs for destinations no longer in perISState.
	for dest, existing := range consolidated {
		if _, inNew := perISState[dest]; inNew {
			continue
		}
		if existing.HasImageSet(isName) {
			orphaned := existing.RemoveImageSet(isName)
			if orphaned {
				delete(consolidated, dest)
			}
		}
	}
}

// loadConsolidatedState initialises m.imageState from the per-MirrorTarget
// ConfigMap. If the ConfigMap is missing or empty it falls back to importing
// the legacy per-IS ConfigMaps so that existing installations survive the
// upgrade without data loss (Phase 3 migration).
// Caller must hold m.mu.
func (m *MirrorManager) loadConsolidatedState(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSets *mirrorv1alpha1.ImageSetList) error {
	state, err := imagestate.LoadForTarget(ctx, m.Client, m.Namespace, m.TargetName)
	if err != nil {
		return fmt.Errorf("load consolidated state: %w", err)
	}
	if len(state) > 0 {
		m.imageState = state
		fmt.Printf("Loaded %d entries from consolidated state ConfigMap\n", len(state))
		return nil
	}

	// Consolidated CM is empty — migrate from legacy per-IS CMs.
	migrated := make(imagestate.ImageState)
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		isState, loadErr := imagestate.Load(ctx, m.Client, m.Namespace, is.Name) //nolint:staticcheck // migration pending
		if loadErr != nil {
			fmt.Printf("Warning: migration: failed to load state for %s: %v\n", is.Name, loadErr)
			continue
		}
		for dest, entry := range isState {
			if entry == nil {
				continue
			}
			ref := imagestate.ImageRef{
				ImageSet:  is.Name,
				Origin:    entry.Origin,
				EntrySig:  entry.EntrySig,
				OriginRef: entry.OriginRef,
			}
			if existing, ok := migrated[dest]; ok {
				existing.AddRef(ref)
			} else {
				e := *entry
				e.Refs = []imagestate.ImageRef{ref}
				e.Origin = ""
				e.EntrySig = ""
				e.OriginRef = ""
				migrated[dest] = &e
			}
		}
	}
	m.imageState = migrated
	if len(migrated) > 0 {
		fmt.Printf("Migrated %d entries from per-IS ConfigMaps to consolidated state\n", len(migrated))
		m.stateDirty = true
	}
	return nil
}
