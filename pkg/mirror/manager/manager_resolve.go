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

	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/cosign"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/graph"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/release"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
	pkgrelease "github.com/mariusbertram/oc-mirror-operator/pkg/release"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// saveCatalogPackages persists catalog package information in two ConfigMaps:
//   - oc-mirror-<target>-<slug>-packages: filtered packages (all selected bundles)
//   - oc-mirror-<target>-<slug>-upstream-packages: upstream packages (channel heads only)
func (m *MirrorManager) saveCatalogPackages(ctx context.Context, slug string, info resources.CatalogInfo, filtered *declcfg.DeclarativeConfig, upstream *declcfg.DeclarativeConfig) error {
	if err := m.writeCatalogPackagesCM(ctx, slug, info, filtered, false); err != nil {
		return fmt.Errorf("filtered packages: %w", err)
	}
	if err := m.writeCatalogPackagesCM(ctx, slug, info, upstream, true); err != nil {
		return fmt.Errorf("upstream packages: %w", err)
	}
	return nil
}

// ensureUpstreamCatalogPackages loads and saves the upstream catalog package CM
// when a cache hit skips ResolveCatalogFull. Only pulls the catalog image if
// the upstream CM does not yet exist. pinnedCatalog is the digest-pinned
// reference (see resolveOperatorSection) used for the actual pull; info
// (and its human-readable info.SourceCatalog) is used only for display/CM
// content.
func (m *MirrorManager) ensureUpstreamCatalogPackages(ctx context.Context, resolver *catalog.CatalogResolver, slug string, info resources.CatalogInfo, pinnedCatalog string) error {
	cmName := fmt.Sprintf("oc-mirror-%s-%s-upstream-packages", m.TargetName, slug)
	existing := &corev1.ConfigMap{}
	err := m.Client.Get(ctx, client.ObjectKey{Name: cmName, Namespace: m.Namespace}, existing)
	if err == nil {
		return nil // already populated
	}
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	upstream, err := resolver.LoadFBC(ctx, pinnedCatalog)
	if err != nil {
		return fmt.Errorf("load upstream FBC: %w", err)
	}
	return m.writeCatalogPackagesCM(ctx, slug, info, upstream, true)
}

func (m *MirrorManager) writeCatalogPackagesCM(ctx context.Context, slug string, info resources.CatalogInfo, cfg *declcfg.DeclarativeConfig, isUpstream bool) error {
	var resp resources.CatalogPackagesResponse
	if isUpstream {
		resp = resources.BuildUpstreamCatalogPackagesResponse(info, cfg)
	} else {
		resp = resources.BuildCatalogPackagesResponse(info, cfg)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	var cmName string
	if isUpstream {
		cmName = fmt.Sprintf("oc-mirror-%s-%s-upstream-packages", m.TargetName, slug)
	} else {
		cmName = fmt.Sprintf("oc-mirror-%s-%s-packages", m.TargetName, slug)
	}

	labelKey := "oc-mirror.openshift.io/catalog-packages"
	if isUpstream {
		labelKey = "oc-mirror.openshift.io/catalog-upstream-packages"
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				labelKey:                              slug,
				"oc-mirror.openshift.io/mirrortarget": m.TargetName,
			},
		},
		Data: map[string]string{"packages.json": string(data)},
	}

	// Set OwnerReference if MirrorTarget is available
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt); err == nil {
		_ = controllerutil.SetControllerReference(mt, cm, m.Scheme)
	}

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
	if mt.UID != "" {
		_ = controllerutil.SetControllerReference(mt, existing, m.Scheme)
	}
	return m.Client.Update(ctx, existing)
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
// The final return value, hadError, reports whether ANY release channel or
// operator catalog entry failed to resolve/probe and had to fall back to
// carrying over its previous state (see carryOverByOriginAndSig call sites in
// resolveReleaseSection/resolveOperatorSection). The caller (manager.go
// Phase B) uses this to avoid marking the ImageSet as successfully polled —
// see the caller's justResolvedISes handling — so a transient probe failure
// does not silently suppress retries for up to a full pollInterval (default
// 24h): without this, a spec change that lands at the same moment as a
// transient upstream hiccup would have its ObservedGeneration/
// LastSuccessfulPollTime advanced anyway, and the newly-added content would
// not be picked up again until the next scheduled poll.
func (m *MirrorManager) resolveImageSet(ctx context.Context, is *mirrorv1alpha1.ImageSet, mt *mirrorv1alpha1.MirrorTarget, currentState imagestate.ImageState) (newState imagestate.ImageState, stateChanged bool, hadError bool, err error) {
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

	newState = make(imagestate.ImageState, len(currentState))
	// Pre-populate with entries this method does NOT own (legacy / unknown
	// origins) so we don't accidentally drop them.
	for dest, entry := range currentState {
		if entry == nil {
			continue
		}
		switch entry.Origin {
		case imagestate.OriginRelease, imagestate.OriginOperator, imagestate.OriginAdditional, imagestate.OriginHelm:
			// owned — handled below
		default:
			cp := *entry
			newState[dest] = &cp
		}
	}

	releaseChanged, releaseHadError, err := m.resolveReleaseSection(ctx, collector, is, mt, currentState, newState, newAnnotations, recollect)
	if err != nil {
		return nil, false, false, fmt.Errorf("resolve releases: %w", err)
	}
	if releaseChanged {
		annotationsChanged = true
	}
	hadError = hadError || releaseHadError

	operatorChanged, operatorHadError, err := m.resolveOperatorSection(ctx, collector, resolver, is, mt, currentState, newState, newAnnotations, recollect)
	if err != nil {
		return nil, false, false, fmt.Errorf("resolve operators: %w", err)
	}
	if operatorChanged {
		annotationsChanged = true
	}
	hadError = hadError || operatorHadError

	// Additional images are cheap to enumerate; always re-collected.
	additional, err := collector.CollectAdditional(ctx, &is.Spec, mt, nil)
	if err != nil {
		return nil, false, false, fmt.Errorf("collect additional images: %w", err)
	}
	mergeIntoStateWithSig(newState, additional, imagestate.OriginAdditional, "", "additional", currentState)

	// Helm charts are re-resolved on every call (no separate digest cache,
	// unlike releases/operators) — chart downloads + template rendering are
	// comparatively cheap and this whole method already only runs on the
	// shouldResolve cadence (spec change or pollInterval), not every reconcile tick.
	helmImages, err := collector.CollectHelm(ctx, &is.Spec, mt, nil)
	if err != nil {
		return nil, false, false, fmt.Errorf("collect helm images: %w", err)
	}
	mergeIntoStateWithSig(newState, helmImages, imagestate.OriginHelm, "", "helm", currentState)

	if m.resolveGraphImage(ctx, is, mt, newAnnotations, recollect) {
		annotationsChanged = true
	}

	// Remove any entry matching spec.mirror.blockedImages, regardless of which
	// origin produced it (including entries carried over from a previous
	// resolution or cache hit). Destinations dropped here lose their ImageSet
	// ref during mergeResolvedIntoConsolidated and are picked up by the
	// existing orphan-cleanup path the same way a removed/narrowed ImageSet
	// entry would be.
	filterBlockedImages(newState, is.Spec.Mirror.BlockedImages)

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
			oclog.Printf("Warning: failed to patch annotations on ImageSet %s: %v\n", is.Name, err)
		}
	}

	stateChanged = !equalState(currentState, newState)
	return newState, stateChanged, hadError, nil
}

// buildCollector returns a Collector + CatalogResolver pair using
// MirrorTarget-aware client (insecure-host config when set).
func (m *MirrorManager) buildCollector(mt *mirrorv1alpha1.MirrorTarget) (*mirror.Collector, *catalog.CatalogResolver) {
	mc := m.registryClientFor(mt)
	return mirror.NewCollector(mc), catalog.New(mc)
}

// registryClientFor returns a MirrorClient configured for mt's target
// registry, using the insecure-host override when set.
func (m *MirrorManager) registryClientFor(mt *mirrorv1alpha1.MirrorTarget) *mirrorclient.MirrorClient {
	if !mt.Spec.Insecure {
		return m.mirrorClient
	}
	host := mt.Spec.Registry
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return mirrorclient.NewMirrorClient([]string{host}, m.authConfigPath)
}

// resolveGraphImage builds and pushes the Cincinnati graph-data image (see
// pkg/mirror/graph) when spec.mirror.platform.graph is set, throttled to the
// MirrorTarget's pollInterval so it isn't rebuilt on every unrelated spec
// change. Returns true if newAnnotations was modified (the caller is
// responsible for persisting it, same as the release/operator sections).
//
// Unlike releases and operators, the graph image isn't tracked as a
// TargetImage/imagestate entry — it's a single side-effect build, not part of
// the per-image Pending/Mirrored/Failed lifecycle, so a timestamp annotation
// is sufficient to track "last built".
func (m *MirrorManager) resolveGraphImage(ctx context.Context, is *mirrorv1alpha1.ImageSet, mt *mirrorv1alpha1.MirrorTarget, newAnnotations map[string]string, recollect bool) bool {
	if !is.Spec.Mirror.Platform.Graph {
		return false
	}

	if !recollect {
		if builtAtStr := newAnnotations[mirrorv1alpha1.GraphImageBuiltAnnotation]; builtAtStr != "" {
			if builtAt, err := time.Parse(time.RFC3339, builtAtStr); err == nil {
				interval, pollingEnabled := effectivePollInterval(mt)
				if !pollingEnabled || time.Since(builtAt) < interval {
					return false
				}
			}
		}
	}

	builder := graph.New(m.registryClientFor(mt))
	digest, err := builder.BuildAndPush(ctx, mt.Spec.Registry)
	if err != nil {
		oclog.Printf("Warning: build graph-data image for %s: %v\n", is.Name, err)
		return false
	}
	oclog.Printf("Graph-data image pushed: %s (digest %s)\n", graph.TargetImage(mt.Spec.Registry), digest)

	newAnnotations[mirrorv1alpha1.GraphImageBuiltAnnotation] = time.Now().UTC().Format(time.RFC3339)
	return true
}

// The second return value, hadError, reports whether any channel failed to
// probe/collect (as opposed to a legitimate cache hit or an empty match set)
// and had to fall back to carrying over its previous state — see
// resolveImageSet's doc comment for why the caller needs this.
func (m *MirrorManager) resolveReleaseSection( //nolint:unparam
	ctx context.Context,
	collector *mirror.Collector,
	is *mirrorv1alpha1.ImageSet,
	mt *mirrorv1alpha1.MirrorTarget,
	currentState imagestate.ImageState,
	newState imagestate.ImageState,
	annotations map[string]string,
	recollect bool,
) (annoChanged bool, hadError bool, err error) { //nolint:unparam
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
			oclog.Printf("Warning: probe release channel %s: %v\n", ch.Name, resolveErr)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			hadError = true
			continue
		}

		verifiedNodes := m.verifyReleaseNodes(ctx, ch, payloadNodes)
		if len(verifiedNodes) == 0 {
			oclog.Printf("Warning: no release nodes for channel %s passed signature verification; skipping\n", ch.Name)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			// Only treat this as a transient failure worth retrying sooner
			// than the next poll when nodes were actually found and rejected
			// by verification — an empty payloadNodes set (no versions in the
			// configured range yet) is a legitimate, stable outcome.
			hadError = hadError || len(payloadNodes) > 0
			continue
		}

		freshSig := release.ResolvedSignature(release.NodeImages(verifiedNodes))
		if !recollect && cached != "" && cached == freshSig {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			continue
		}

		images, err := collector.CollectReleasesForChannel(ctx, &is.Spec, mt, ch, verifiedNodes)
		if err != nil {
			oclog.Printf("Warning: collect release channel %s: %v\n", ch.Name, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginRelease, sig, originRef)
			hadError = true
			continue
		}
		mergeIntoStateWithSig(newState, images, imagestate.OriginRelease, sig, originRef, currentState)

		// Persist the (already downloaded and verified) GPG signatures for the
		// Resource API. Failures here are best-effort — they do not block the
		// main mirroring flow, since verification already happened above.
		m.downloadSignaturesForNodes(ctx, verifiedNodes)

		if annotations[annoKey] != freshSig {
			annotations[annoKey] = freshSig
			annoChanged = true
		}
	}
	return annoChanged, hadError, nil
}

// signatureConfigMapName returns the name of the ConfigMap that stores release
// GPG signatures for this MirrorTarget.
func (m *MirrorManager) signatureConfigMapName() string {
	return m.TargetName + "-signatures"
}

// verifyReleaseNodes downloads and cryptographically verifies the GPG
// signature of each release node against the embedded Red Hat release
// signing keys, returning only the nodes that verify successfully. A node
// whose signature cannot be downloaded or fails verification is dropped
// (logged, not mirrored) unless ch.SkipSignatureVerification is set.
//
// This runs before CollectReleasesForChannel/mergeIntoStateWithSig so a
// tampered or unsigned release payload is never queued for mirroring in the
// first place, rather than merely being flagged after the fact.
func (m *MirrorManager) verifyReleaseNodes(ctx context.Context, ch mirrorv1alpha1.ReleaseChannel, nodes []release.Node) []release.Node {
	if ch.SkipSignatureVerification || len(nodes) == 0 {
		return nodes
	}

	sigClient := pkgrelease.NewSignatureClient(nil)
	verified := make([]release.Node, 0, len(nodes))
	for _, node := range nodes {
		digest := extractDigest(node.Image)
		if digest == "" {
			oclog.Printf("Warning: release node %s (%s) has no digest, cannot verify signature; skipping\n", ch.Name, node.Image)
			continue
		}
		sigData, err := sigClient.DownloadSignature(ctx, digest)
		if err != nil {
			oclog.Printf("Warning: download signature for %s (channel %s): %v; skipping until signed\n", digest, ch.Name, err)
			continue
		}
		if err := pkgrelease.VerifySignature(sigData, digest); err != nil {
			oclog.Printf("Warning: signature verification failed for %s (channel %s): %v; skipping\n", digest, ch.Name, err)
			continue
		}
		verified = append(verified, node)
	}
	return verified
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
			oclog.Printf("Warning: download signature for %s: %v\n", digest, err)
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

	mt := &mirrorv1alpha1.MirrorTarget{}
	_ = m.Client.Get(ctx, client.ObjectKey{Name: m.TargetName, Namespace: m.Namespace}, mt)

	if existing.Name == "" {
		existing.Name = cmName
		existing.Namespace = m.Namespace
		existing.ObjectMeta = metav1.ObjectMeta{Name: cmName, Namespace: m.Namespace}
		if mt.UID != "" {
			_ = controllerutil.SetControllerReference(mt, existing, m.Scheme)
		}
		if err := m.Client.Create(ctx, existing); err != nil && !apierrors.IsAlreadyExists(err) {
			oclog.Printf("Warning: create signatures ConfigMap: %v\n", err)
		}
		return
	}
	if mt.UID != "" {
		_ = controllerutil.SetControllerReference(mt, existing, m.Scheme)
	}
	if err := m.Client.Update(ctx, existing); err != nil {
		oclog.Printf("Warning: update signatures ConfigMap: %v\n", err)
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

// The second return value, hadError, reports whether any operator entry
// failed to probe/resolve (as opposed to a legitimate cache hit) and had to
// fall back to carrying over its previous state — see resolveImageSet's doc
// comment for why the caller needs this.
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
) (annoChanged bool, hadError bool, err error) { //nolint:unparam
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
			oclog.Printf("Warning: probe catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			hadError = true
			continue
		}

		if op.SignatureVerification != nil {
			if err := m.verifyOperatorCatalogSignature(ctx, mt, op, freshDigest); err != nil {
				oclog.Printf("Warning: signature verification failed for catalog %s: %v; skipping until signed\n", op.Catalog, err)
				carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
				hadError = true
				continue
			}
		}

		// Pin the catalog reference to the digest just probed above, so a tag
		// that moves between this point and the ResolveCatalogFull/LoadFBC pull
		// below cannot cause the FBC parse to see different content than what
		// freshDigest (and the resulting cache annotation) reflects. Falls back
		// to the unpinned (tag) reference if the reference cannot be parsed —
		// same behavior as before this pinning was added.
		pinnedCatalog, pinErr := resolver.PinDigest(op.Catalog, freshDigest)
		if pinErr != nil {
			oclog.Printf("Warning: pin catalog digest for %s: %v\n", op.Catalog, pinErr)
			pinnedCatalog = op.Catalog
		}

		catSlug := resources.CatalogSlug(op.Catalog)
		targetImage := resources.CatalogTargetImage(mt.Spec.Registry, op)
		catInfo := resources.CatalogInfo{
			SourceCatalog: op.Catalog,
			TargetImage:   targetImage,
			DisplayName:   catSlug,
		}

		cacheToken := mirrorv1alpha1.OperatorCacheValue(freshDigest)
		if !recollect && mirrorv1alpha1.OperatorCacheHit(cached, freshDigest) {
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			// Ensure upstream packages CM exists even on a cache hit. This handles
			// the first run after the feature was added (existing clusters).
			if err := m.ensureUpstreamCatalogPackages(ctx, resolver, catSlug, catInfo, pinnedCatalog); err != nil {
				oclog.Printf("Warning: failed to ensure upstream catalog packages for %s: %v\n", catSlug, err)
			}
			continue
		}

		images, filtered, upstream, err := resolver.ResolveCatalogFull(ctx, pinnedCatalog, op.Packages)
		if err != nil {
			oclog.Printf("Warning: collect catalog %s: %v\n", op.Catalog, err)
			carryOverByOriginAndSig(currentState, newState, imagestate.OriginOperator, sig, originRef)
			hadError = true
			continue
		}

		targetImages := make([]mirror.TargetImage, 0, len(images))
		for img, info := range images {
			targetImages = append(targetImages, mirror.TargetImage{
				Source:        img,
				BundleRef:     info.Label,
				IsBundleImage: info.IsBundleImage,
				Destination:   mirror.ComponentDestination(mt.Spec.Registry, img),
			})
		}
		mergeIntoStateWithSig(newState, targetImages, imagestate.OriginOperator, sig, originRef, currentState)

		if err := m.saveCatalogPackages(ctx, catSlug, catInfo, filtered, upstream); err != nil {
			oclog.Printf("Warning: failed to save catalog packages for %s: %v\n", catSlug, err)
		}

		if annotations[annoKey] != cacheToken {
			annotations[annoKey] = cacheToken
			annoChanged = true
		}
	}
	return annoChanged, hadError, nil
}

// verifyOperatorCatalogSignature verifies op.Catalog's cosign/sigstore
// signature at catalogDigest against the public key referenced by
// op.SignatureVerification. Unlike release payloads (which are always
// verified against embedded Red Hat keys), this is opt-in per catalog since
// there is no single trusted signer for third-party operator catalogs.
func (m *MirrorManager) verifyOperatorCatalogSignature(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, op mirrorv1alpha1.Operator, catalogDigest string) error {
	sv := op.SignatureVerification
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: m.Namespace, Name: sv.PublicKeySecretRef.Name}
	if err := m.Client.Get(ctx, secretKey, secret); err != nil {
		return fmt.Errorf("get public key secret %s: %w", sv.PublicKeySecretRef.Name, err)
	}
	pubKeyPEM, ok := secret.Data[sv.PublicKeySecretRef.Key]
	if !ok {
		return fmt.Errorf("secret %s has no key %q", sv.PublicKeySecretRef.Name, sv.PublicKeySecretRef.Key)
	}
	return cosign.VerifyImageSignature(ctx, m.registryClientFor(mt), op.Catalog, catalogDigest, pubKeyPEM)
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
			Source:        img.Source,
			State:         "Pending",
			Origin:        origin,
			EntrySig:      sig,
			OriginRef:     ref,
			IsBundleImage: img.IsBundleImage,
		}
		if existing, ok := prev[img.Destination]; ok && existing != nil && (existing.Origin == origin || existing.Origin == "") {
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

// filterBlockedImages deletes any state entry whose Source matches a
// configured spec.mirror.blockedImages name. See mirror.ImageBlocked for the
// matching rules (registry-agnostic path, with optional tag/digest narrowing).
func filterBlockedImages(state imagestate.ImageState, blocked []mirrorv1alpha1.BlockedImage) {
	if len(blocked) == 0 {
		return
	}
	for dest, entry := range state {
		if entry == nil {
			continue
		}
		for _, b := range blocked {
			if mirror.ImageBlocked(entry.Source, b.Name) {
				delete(state, dest)
				break
			}
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
				k == mirrorv1alpha1.GraphImageBuiltAnnotation ||
				k == mirrorv1alpha1.RecollectAnnotation {
				delete(fresh.Annotations, k)
			}
		}
		for k, v := range desired {
			if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) ||
				strings.HasPrefix(k, mirrorv1alpha1.ReleaseDigestAnnotationPrefix) ||
				k == mirrorv1alpha1.GraphImageBuiltAnnotation {
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
	pollInterval, pollingEnabled := effectivePollInterval(mt)
	if !pollingEnabled {
		return false
	}
	if is.Status.LastSuccessfulPollTime == nil {
		return true
	}
	return time.Since(is.Status.LastSuccessfulPollTime.Time) >= pollInterval
}

// effectivePollInterval returns the effective upstream-polling interval for
// mt (default 24h, floored to 1h when explicitly set) and whether polling is
// enabled at all (mt.Spec.PollInterval == "0" disables it).
func effectivePollInterval(mt *mirrorv1alpha1.MirrorTarget) (time.Duration, bool) {
	pollInterval := 24 * time.Hour
	if mt.Spec.PollInterval == nil {
		return pollInterval, true
	}
	if mt.Spec.PollInterval.Duration <= 0 {
		return 0, false
	}
	pollInterval = mt.Spec.PollInterval.Duration
	if pollInterval < 1*time.Hour {
		pollInterval = 1 * time.Hour
	}
	return pollInterval, true
}

// hasStaleCacheAnnotations returns true if any catalog-digest cache annotation
// on the ImageSet was written with an older mirrorv1alpha1.OperatorCacheVersion.
// This forces re-resolution after an operator binary upgrade that changed the
// filtering logic (e.g. heads-only), even if the ImageSet spec itself hasn't
// changed.
func hasStaleCacheAnnotations(is *mirrorv1alpha1.ImageSet) bool {
	if is.Annotations == nil {
		return false
	}
	prefix := mirrorv1alpha1.OperatorCacheVersion + ":"
	for k, v := range is.Annotations {
		if strings.HasPrefix(k, mirrorv1alpha1.CatalogDigestAnnotationPrefix) {
			if !strings.HasPrefix(v, prefix) {
				return true
			}
		}
	}
	return false
}

// filterByImageSet returns a per-IS view of the manager's live in-memory
// state (m.imageState), scoped to destinations currently owned by isName per
// the owners map. Unlike the pre-partitioning consolidated model, entries
// here already carry their own scoped Origin/EntrySig/OriginRef directly —
// there is no Refs promotion step, since ownership lives out-of-band in
// owners rather than inside the entry.
func filterByImageSet(state imagestate.ImageState, owners map[string][]string, isName string) imagestate.ImageState {
	result := make(imagestate.ImageState, len(state)/2)
	for dest, entry := range state {
		if entry == nil || !hasOwner(owners, dest, isName) {
			continue
		}
		cp := *entry
		result[dest] = &cp
	}
	return result
}

// resetImageSetToPendingLocked resets every image owned by isName back to
// "Pending", regardless of its current state — including already-"Mirrored"
// entries — so Phase D/E of reconcile() re-dispatches all of them to worker
// pods this tick. Used by the ForceResyncAnnotation trigger, which (unlike
// RecollectAnnotation) forces a complete re-verification/re-transfer
// independent of what state each image is currently in.
//
// PermanentlyFailed is intentionally left untouched: per ImageEntry's doc
// comment it is a sticky marker that is never cleared once set, even across
// a Pending retry, so catalog-build gating and failedImageDetails keep
// surfacing the image's history through the resync.
//
// Entries currently being processed by an in-flight worker batch
// (m.inProgress) are left untouched — the worker's eventual callback will
// still mark them Mirrored/Failed as normal, and resetting them here would
// just race that callback without accomplishing anything.
//
// Caller must hold m.mu. Returns true if any entry was changed.
func (m *MirrorManager) resetImageSetToPendingLocked(isName string) bool {
	changed := false
	for dest, entry := range m.imageState {
		if entry == nil || !hasOwner(m.owners, dest, isName) {
			continue
		}
		if m.inProgress[dest] != "" {
			continue
		}
		if entry.State == statePending && entry.RetryCount == 0 && entry.LastError == "" && !entry.SignatureVerified {
			continue
		}
		entry.State = statePending
		entry.RetryCount = 0
		entry.LastError = ""
		entry.SignatureVerified = false
		m.mirrored[dest] = false
		changed = true
	}
	return changed
}

// clearForceResyncAnnotation removes the one-shot ForceResyncAnnotation from
// the ImageSet after resetImageSetToPendingLocked has applied its effect, so
// it doesn't keep re-triggering on every subsequent reconcile. Runs outside
// m.mu — it's a network call, matching patchImageSetAnnotations.
func (m *MirrorManager) clearForceResyncAnnotation(ctx context.Context, is *mirrorv1alpha1.ImageSet) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &mirrorv1alpha1.ImageSet{}
		if err := m.Client.Get(ctx, client.ObjectKey{Namespace: is.Namespace, Name: is.Name}, fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if _, ok := fresh.Annotations[mirrorv1alpha1.ForceResyncAnnotation]; !ok {
			return nil
		}
		delete(fresh.Annotations, mirrorv1alpha1.ForceResyncAnnotation)
		return m.Client.Update(ctx, fresh)
	})
}

// hasOwner reports whether isName is among owners[dest].
func hasOwner(owners map[string][]string, dest, isName string) bool {
	for _, n := range owners[dest] {
		if n == isName {
			return true
		}
	}
	return false
}

// addOwner records isName as an owner of dest, deduplicating.
func addOwner(owners map[string][]string, dest, isName string) {
	if hasOwner(owners, dest, isName) {
		return
	}
	owners[dest] = append(owners[dest], isName)
}

// removeOwner removes isName from owners[dest]. Returns true if isName was
// present (and has now been removed).
func removeOwner(owners map[string][]string, dest, isName string) bool {
	names := owners[dest]
	out := names[:0]
	removed := false
	for _, n := range names {
		if n == isName {
			removed = true
			continue
		}
		out = append(out, n)
	}
	if !removed {
		return false
	}
	if len(out) == 0 {
		delete(owners, dest)
	} else {
		owners[dest] = out
	}
	return true
}

// mergeResolvedIntoConsolidated integrates a freshly-resolved per-IS state
// into the manager's live in-memory state and owners map. It:
//   - Adds/updates isName's ownership of each destination present in perISState.
//   - Removes isName's ownership from destinations no longer in perISState.
//
// Global entry fields (State, RetryCount, LastError, PermanentlyFailed) are
// preserved for existing entries — only Source and ownership are updated.
// Destinations that lose their last owner are reported via the returned
// slice so the caller (manager.go Phase D) can move them into the pending
// orphans snapshot for the MirrorTarget controller's cleanup Job — they are
// intentionally left in `state` for the caller to remove.
func mergeResolvedIntoConsolidated(state imagestate.ImageState, owners map[string][]string, perISState imagestate.ImageState, isName string) (orphaned []string) {
	// Step 1: Upsert entries that exist in the new per-IS state.
	for dest, newEntry := range perISState {
		if newEntry == nil {
			continue
		}
		if existing, ok := state[dest]; ok {
			if existing.Source != newEntry.Source {
				existing.Source = newEntry.Source
			}
		} else {
			e := *newEntry
			state[dest] = &e
		}
		addOwner(owners, dest, isName)
	}

	// Step 2: Remove stale ownership for destinations no longer in perISState.
	for dest := range state {
		if _, inNew := perISState[dest]; inNew {
			continue
		}
		if !removeOwner(owners, dest, isName) {
			continue
		}
		if len(owners[dest]) == 0 {
			orphaned = append(orphaned, dest)
		}
	}
	return orphaned
}

// loadPartitionedState initialises m.imageState and m.owners by loading each
// referenced ImageSet's own state ConfigMap and merging them into the
// manager's single in-memory working set — kept dest-keyed for O(1) worker
// status/should-mirror lookups. m.owners tracks which ImageSet(s) each
// destination currently belongs to, mirroring (without persisting as) the
// on-disk shared-image index. Runs the one-time legacy-consolidated-ConfigMap
// migration first.
// Caller must hold m.mu.
func (m *MirrorManager) loadPartitionedState(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, imageSets *mirrorv1alpha1.ImageSetList) {
	if err := imagestate.MigrateConsolidatedToPerImageSet(ctx, m.Client, m.Namespace, m.TargetName, mt, m.Scheme); err != nil {
		oclog.Printf("Warning: failed to migrate legacy consolidated state: %v\n", err)
	}

	imageState := make(imagestate.ImageState)
	owners := make(map[string][]string)
	loaded := 0
	for _, is := range imageSets.Items {
		if !containsString(mt.Spec.ImageSets, is.Name) {
			continue
		}
		isState, loadErr := imagestate.Load(ctx, m.Client, m.Namespace, is.Name)
		if loadErr != nil {
			oclog.Printf("Warning: failed to load state for ImageSet %s: %v\n", is.Name, loadErr)
			continue
		}
		for dest, entry := range isState {
			if entry == nil {
				continue
			}
			addOwner(owners, dest, is.Name)
			imageState[dest] = mergeLoadedEntry(imageState[dest], entry)
		}
		loaded += len(isState)
	}
	m.imageState = imageState
	m.owners = owners
	if loaded > 0 {
		oclog.Printf("Loaded %d image entries across %d ImageSets\n", len(imageState), len(imageSets.Items))
	}
}

// mergeLoadedEntry resolves a rare divergence between two ImageSets' own
// copies of a shared destination (possible only if a manager crash landed
// between writing some but not all owners during a prior flush — writes
// across owning ConfigMaps are not atomic, see
// docs/design/imagestate-per-imageset-partitioning.md §4): prefer whichever
// copy reflects more progress (Mirrored, then higher RetryCount).
func mergeLoadedEntry(existing, incoming *imagestate.ImageEntry) *imagestate.ImageEntry {
	if existing == nil {
		return incoming
	}
	if incoming.State == stateMirrored || existing.State == stateMirrored {
		if incoming.State == stateMirrored {
			return incoming
		}
		return existing
	}
	if incoming.RetryCount > existing.RetryCount {
		return incoming
	}
	return existing
}
