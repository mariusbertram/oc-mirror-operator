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
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
)

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

func (m *MirrorManager) resolveReleaseSection(
	ctx context.Context,
	collector *mirror.Collector,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	currentState imagestate.ImageState,
	newState imagestate.ImageState,
	annotations map[string]string,
	recollect bool,
) (bool, error) {
	annoChanged := false
	arch := is.Spec.Mirror.Platform.Architectures
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}

	for _, ch := range is.Spec.Mirror.Platform.Channels {
		sig := mirrorv1alpha1.ReleaseChannelSignature(ch, arch, is.Spec.Mirror.Platform.KubeVirtContainer)
		annoKey := mirrorv1alpha1.ReleaseDigestAnnotationKey(sig)
		cached := annotations[annoKey]

		payloadImages, resolveErr := collector.ResolveReleasePayloadImages(ctx, ch, arch)
		if resolveErr != nil {
			fmt.Printf("Warning: probe release channel %s: %v\n", ch.Name, resolveErr)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig)
			continue
		}
		freshSig := release.ResolvedSignature(payloadImages)
		if !recollect && cached != "" && cached == freshSig {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig)
			continue
		}

		images, err := collector.CollectReleasesForChannel(ctx, &is.Spec, mt, ch, payloadImages)
		if err != nil {
			fmt.Printf("Warning: collect release channel %s: %v\n", ch.Name, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig)
			continue
		}
		originRef := fmt.Sprintf("%s [%s]", ch.Name, strings.Join(arch, ","))
		mergeIntoStateWithSig(newState, images, imagestate.OriginRelease, sig, originRef, currentState)

		if annotations[annoKey] != freshSig {
			annotations[annoKey] = freshSig
			annoChanged = true
		}
	}
	return annoChanged, nil
}

func (m *MirrorManager) resolveOperatorSection(
	ctx context.Context,
	collector *mirror.Collector,
	resolver *catalog.CatalogResolver,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	currentState imagestate.ImageState,
	newState imagestate.ImageState,
	annotations map[string]string,
	recollect bool,
) (bool, error) {
	annoChanged := false

	for _, op := range is.Spec.Mirror.Operators {
		sig := mirrorv1alpha1.OperatorEntrySignature(op)
		annoKey := mirrorv1alpha1.CatalogDigestAnnotationKey(sig)
		cached := annotations[annoKey]

		freshDigest, err := resolver.GetCatalogDigest(ctx, op.Catalog)
		if err != nil {
			fmt.Printf("Warning: probe catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig)
			continue
		}

		if !recollect && cached != "" && cached == freshDigest {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig)
			continue
		}

		images, err := collector.CollectOperatorEntry(ctx, op, mt)
		if err != nil {
			fmt.Printf("Warning: collect catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig)
			continue
		}
		pkgNames := make([]string, 0, len(op.Packages))
		for _, p := range op.Packages {
			pkgNames = append(pkgNames, p.Name)
		}
		sort.Strings(pkgNames)
		originRef := op.Catalog
		if len(pkgNames) > 0 {
			originRef = fmt.Sprintf("%s [%s]", op.Catalog, strings.Join(pkgNames, ", "))
		}
		mergeIntoStateWithSig(newState, images, imagestate.OriginOperator, sig, originRef, currentState)

		if annotations[annoKey] != freshDigest {
			annotations[annoKey] = freshDigest
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
		entry := &imagestate.ImageEntry{
			Source:    img.Source,
			State:     "Pending",
			Origin:    origin,
			EntrySig:  sig,
			OriginRef: originRef,
		}
		if existing, ok := prev[img.Destination]; ok && existing != nil && existing.Origin == origin {
			if existing.State == "Mirrored" || existing.State == "Failed" {
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
// Backward-compat: entries with empty EntrySig are treated as legacy and
// carried over for any sig matching their Origin so that older state is not
// dropped on first migration. They will be re-tagged with a real sig the
// next time their owning spec entry is re-resolved.
func carryOverByOriginAndSig(src, dst imagestate.ImageState, origin imagestate.ImageOrigin, sig string) {
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
		if va.Source != vb.Source || va.State != vb.State || va.Origin != vb.Origin || va.EntrySig != vb.EntrySig || va.RetryCount != vb.RetryCount || va.LastError != vb.LastError {
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
		}
	}
	return resolved
}

// shouldResolve gates resolveImageSet calls so we don't hammer upstream
// registries on every 30s reconcile loop. Resolution runs when:
//   - imagestate is empty (initial resolution)
//   - the recollect annotation is set
//   - the spec has changed (Generation > Status.ObservedGeneration)
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
