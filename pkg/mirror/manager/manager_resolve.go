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

// operatorCacheHit returns true if the cached annotation value matches the
// current cache token for the given digest.
func operatorCacheHit(cached, digest string) bool {
	return cached != "" && cached == operatorCacheValue(digest)
}

// resolveImageSet enumerates the upstream content (releases, operator
// catalogs, additional images) referenced by an ImageSet, merges the result
// into the consolidated imagestate, and refreshes the digest cache
// annotations.
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

	// Create a fresh state for this resolution run, carrying over existing entries.
	newState := make(imagestate.ImageState, len(currentState))
	for dest, entry := range currentState {
		cp := *entry
		newState[dest] = &cp
	}

	// Track which destinations are still part of this ImageSet.
	visitedDests := make(map[string]bool)

	if m.resolveReleaseSection(ctx, collector, is, mt, currentState, newState, newAnnotations, recollect, visitedDests) {
		annotationsChanged = true
	}

	if m.resolveOperatorSection(ctx, collector, resolver, is, mt, currentState, newState, newAnnotations, recollect, visitedDests) {
		annotationsChanged = true
	}

	// Additional images are cheap to enumerate; always re-collected.
	additional, err := collector.CollectAdditional(ctx, &is.Spec, mt, nil)
	if err != nil {
		return nil, false, fmt.Errorf("collect additional images: %w", err)
	}
	mergeIntoStateWithSig(newState, additional, imagestate.OriginAdditional, "", "additional", is.Name, currentState, visitedDests)

	// Post-process newState: remove refs for this IS from any destination NOT in visitedDests.
	for dest, entry := range newState {
		if entry.HasImageSet(is.Name) && !visitedDests[dest] {
			entry.RemoveImageSet(is.Name)
		}
	}

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
	visited map[string]bool,
) bool {
	annoChanged := false
	arch := is.Spec.Mirror.Platform.Architectures
	if len(arch) == 0 {
		arch = []string{"amd64"}
	}

	sigClient := release.NewSignatureClient()
	signatures := make(map[string][]byte)

	for _, ch := range is.Spec.Mirror.Platform.Channels {
		sig := mirrorv1alpha1.ReleaseChannelSignature(ch, arch, is.Spec.Mirror.Platform.KubeVirtContainer)
		annoKey := mirrorv1alpha1.ReleaseDigestAnnotationKey(sig)
		cached := annotations[annoKey]
		originRef := fmt.Sprintf("%s [%s]", ch.Name, strings.Join(arch, ","))

		payloadNodes, resolveErr := collector.ResolveReleasePayloadNodes(ctx, ch, arch)
		if resolveErr != nil {
			fmt.Printf("Warning: probe release channel %s: %v\n", ch.Name, resolveErr)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef, is.Name, visited)
			continue
		}

		// Download signatures for each resolved node.
		for _, node := range payloadNodes {
			parts := strings.Split(node.Image, "@")
			if len(parts) != 2 {
				continue
			}
			digest := parts[1]
			if _, exists := signatures[digest]; !exists {
				if sign, err := sigClient.DownloadSignature(ctx, digest); err == nil {
					signatures[digest] = sign
				} else {
					fmt.Printf("Warning: failed to download signature for %s: %v\n", digest, err)
				}
			}
		}

		freshSig := release.ResolvedSignature(release.NodeImages(payloadNodes))
		if !recollect && cached != "" && cached == freshSig {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef, is.Name, visited)
			continue
		}

		images, err := collector.CollectReleasesForChannel(ctx, &is.Spec, mt, ch, payloadNodes)
		if err != nil {
			fmt.Printf("Warning: collect release channel %s: %v\n", ch.Name, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef, is.Name, visited)
			continue
		}
		mergeIntoStateWithSig(newState, images, imagestate.OriginRelease, sig, originRef, is.Name, currentState, visited)

		if annotations[annoKey] != freshSig {
			annotations[annoKey] = freshSig
			annoChanged = true
		}
	}

	// Persist signatures in ConfigMap.
	if len(signatures) > 0 {
		if err := m.saveSignatures(ctx, mt, signatures); err != nil {
			fmt.Printf("Warning: failed to save release signatures: %v\n", err)
		}
	}

	return annoChanged
}

func (m *MirrorManager) saveSignatures(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, signatures map[string][]byte) error {
	cmName := mt.Name + "-signatures"
	existing := &corev1.ConfigMap{}
	getErr := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: cmName}, existing)

	binaryData := make(map[string][]byte)
	if getErr == nil {
		for k, v := range existing.BinaryData {
			binaryData[k] = v
		}
	}

	// Merge new signatures (digest sha256:hash -> key sha256-hash).
	for digest, data := range signatures {
		key := strings.Replace(digest, ":", "-", 1)
		binaryData[key] = data
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: m.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         mirrorv1alpha1.GroupVersion.String(),
				Kind:               "MirrorTarget",
				Name:               mt.Name,
				UID:                mt.UID,
				Controller:         ptr(true),
				BlockOwnerDeletion: ptr(false),
			}},
		},
		BinaryData: binaryData,
	}

	if apierrors.IsNotFound(getErr) {
		return m.Client.Create(ctx, cm)
	}
	if getErr != nil {
		return getErr
	}
	cm.ResourceVersion = existing.ResourceVersion
	return m.Client.Update(ctx, cm)
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
	visited map[string]bool,
) bool {
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
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef, is.Name, visited)
			continue
		}

		cacheToken := operatorCacheValue(freshDigest)
		if !recollect && operatorCacheHit(cached, freshDigest) {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef, is.Name, visited)
			continue
		}

		images, err := collector.CollectOperatorEntry(ctx, op, mt)
		if err != nil {
			fmt.Printf("Warning: collect catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef, is.Name, visited)
			continue
		}
		mergeIntoStateWithSig(newState, images, imagestate.OriginOperator, sig, originRef, is.Name, currentState, visited)

		if annotations[annoKey] != cacheToken {
			annotations[annoKey] = cacheToken
			annoChanged = true
		}
	}
	return annoChanged
}

// mergeIntoStateWithSig writes entries into dst, tagging each with origin+sig for the given imageSet.
func mergeIntoStateWithSig(dst imagestate.ImageState, images []mirror.TargetImage, origin imagestate.ImageOrigin, sig, originRef, isName string, prev imagestate.ImageState, visited map[string]bool) {
	for _, img := range images {
		visited[img.Destination] = true
		ref := originRef
		if img.BundleRef != "" {
			ref = fmt.Sprintf("%s — %s", originRef, img.BundleRef)
		}

		entry, ok := dst[img.Destination]
		if !ok {
			entry = &imagestate.ImageEntry{
				Source: img.Source,
				State:  "Pending",
			}
			dst[img.Destination] = entry

			if existing, exists := prev[img.Destination]; exists && existing != nil {
				if existing.State == "Mirrored" && existing.Origin == origin {
					entry.State = existing.State
					entry.RetryCount = existing.RetryCount
					entry.LastError = existing.LastError
					entry.PermanentlyFailed = existing.PermanentlyFailed
				}
			}

		}

		entry.AddRef(imagestate.ImageRef{
			ImageSet:  isName,
			Origin:    origin,
			EntrySig:  sig,
			OriginRef: ref,
		})
		// Backward compat
		entry.Origin = origin
		entry.EntrySig = sig
		entry.OriginRef = ref
	}
}

// carryOverByOriginAndSig copies references from src into dst that match (origin, sig) for the given imageSet.
func carryOverByOriginAndSig(src, dst imagestate.ImageState, origin imagestate.ImageOrigin, sig, originRef, isName string, visited map[string]bool) {
	for dest, srcEntry := range src {
		if srcEntry == nil {
			continue
		}

		// Find the reference in src that matches this IS and sig.
		var foundRef *imagestate.ImageRef
		for _, r := range srcEntry.Refs {
			if r.ImageSet == isName && r.Origin == origin {
				if r.EntrySig == "" || r.EntrySig == sig {
					foundRef = &r
					break
				}
			}
		}

		// Fallback for legacy entries (Phase 3 migration)
		if foundRef == nil && srcEntry.Origin == origin && (srcEntry.EntrySig == "" || srcEntry.EntrySig == sig) {
			foundRef = &imagestate.ImageRef{
				ImageSet:  isName,
				Origin:    srcEntry.Origin,
				EntrySig:  srcEntry.EntrySig,
				OriginRef: srcEntry.OriginRef,
			}
		}

		if foundRef == nil {
			continue
		}

		visited[dest] = true
		dstEntry, ok := dst[dest]
		if !ok {
			dstEntry = &imagestate.ImageEntry{
				Source:            srcEntry.Source,
				State:             srcEntry.State,
				LastError:         srcEntry.LastError,
				RetryCount:        srcEntry.RetryCount,
				PermanentlyFailed: srcEntry.PermanentlyFailed,
				// Backward compat
				Origin:    srcEntry.Origin,
				EntrySig:  srcEntry.EntrySig,
				OriginRef: srcEntry.OriginRef,
			}
			dst[dest] = dstEntry
		}

		ref := *foundRef
		if ref.EntrySig == "" {
			ref.EntrySig = sig
		}
		if ref.OriginRef == "" {
			ref.OriginRef = originRef
		}
		dstEntry.AddRef(ref)

		// Backward compat update if needed
		if dstEntry.EntrySig == "" {
			dstEntry.EntrySig = ref.EntrySig
		}
		if dstEntry.OriginRef == "" {
			dstEntry.OriginRef = ref.OriginRef
		}
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
		if va.Source != vb.Source ||
			va.State != vb.State ||
			va.RetryCount != vb.RetryCount ||
			va.LastError != vb.LastError ||
			va.Origin != vb.Origin ||
			va.EntrySig != vb.EntrySig ||
			va.OriginRef != vb.OriginRef ||
			len(va.Refs) != len(vb.Refs) {
			return false
		}
		for i := range va.Refs {
			if va.Refs[i].ImageSet != vb.Refs[i].ImageSet ||
				va.Refs[i].Origin != vb.Refs[i].Origin ||
				va.Refs[i].EntrySig != vb.Refs[i].EntrySig ||
				va.Refs[i].OriginRef != vb.Refs[i].OriginRef {
				return false
			}
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
		entry.Refs = make([]imagestate.ImageRef, len(v.Refs))
		copy(entry.Refs, v.Refs)
		out[k] = &entry
	}
	return out
}

// mergeWorkerUpdates applies any State/RetryCount/LastError changes from live into resolved.
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

// shouldResolve gates resolveImageSet calls.
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

// hasStaleCacheAnnotations returns true if any catalog-digest cache annotation is outdated.
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

func ptr[T any](v T) *T { return &v }
