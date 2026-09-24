package manager

import "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"

// specMeta is the part of an ImageEntry that describes which spec entry of an
// ImageSet produced the destination (as opposed to the lifecycle fields, which
// describe the one underlying mirrored image). A destination shared by several
// ImageSets is produced by a different spec entry in each of them, so this
// metadata is kept per owner (MirrorManager.ownerMeta) instead of only on the
// single shared ImageEntry — otherwise every owner but one would see a foreign
// EntrySig on its next cache hit, drop the destination from its resolved view
// and lose ownership of an image its spec still needs (#160).
type specMeta struct {
	Origin        imagestate.ImageOrigin
	EntrySig      string
	OriginRef     string
	Catalog       string
	IsBundleImage bool
}

func specMetaOf(e *imagestate.ImageEntry) specMeta {
	return specMeta{Origin: e.Origin, EntrySig: e.EntrySig, OriginRef: e.OriginRef, Catalog: e.Catalog, IsBundleImage: e.IsBundleImage}
}

func (s specMeta) applyTo(e *imagestate.ImageEntry) {
	e.Origin = s.Origin
	e.EntrySig = s.EntrySig
	e.OriginRef = s.OriginRef
	e.Catalog = s.Catalog
	e.IsBundleImage = s.IsBundleImage
}

// ownerView returns entry as isName sees it: a copy carrying isName's own
// spec metadata when recorded, otherwise a plain copy.
func (m *MirrorManager) ownerView(dest, isName string, entry *imagestate.ImageEntry) *imagestate.ImageEntry {
	cp := *entry
	if meta, ok := m.ownerMeta[dest][isName]; ok {
		meta.applyTo(&cp)
	}
	return &cp
}

// setOwnerMetaLocked records isName's spec metadata for dest.
// Caller must hold m.mu.
func (m *MirrorManager) setOwnerMetaLocked(dest, isName string, meta specMeta) {
	if m.ownerMeta == nil {
		m.ownerMeta = make(map[string]map[string]specMeta)
	}
	if m.ownerMeta[dest] == nil {
		m.ownerMeta[dest] = make(map[string]specMeta)
	}
	m.ownerMeta[dest][isName] = meta
}

// recordOwnerMetaLocked makes isName's recorded spec metadata match a freshly
// resolved per-ImageSet state: set for every destination in resolved, removed
// for every destination isName no longer resolves to. Caller must hold m.mu.
func (m *MirrorManager) recordOwnerMetaLocked(isName string, resolved imagestate.ImageState) {
	for dest, perOwner := range m.ownerMeta {
		if _, still := resolved[dest]; still {
			continue
		}
		delete(perOwner, isName)
		if len(perOwner) == 0 {
			delete(m.ownerMeta, dest)
		}
	}
	for dest, entry := range resolved {
		if entry != nil {
			m.setOwnerMetaLocked(dest, isName, specMetaOf(entry))
		}
	}
}

// pruneOwnerMetaLocked drops recorded metadata of owners dest no longer has
// (all of it when dest has no owner left). Caller must hold m.mu.
func (m *MirrorManager) pruneOwnerMetaLocked(dest string) {
	perOwner, ok := m.ownerMeta[dest]
	if !ok {
		return
	}
	for isName := range perOwner {
		if !hasOwner(m.owners, dest, isName) {
			delete(perOwner, isName)
		}
	}
	if len(perOwner) == 0 {
		delete(m.ownerMeta, dest)
	}
}

// filterByImageSetLocked returns isName's view of the live state, see
// filterByImageSet. Caller must hold m.mu.
func (m *MirrorManager) filterByImageSetLocked(isName string) imagestate.ImageState {
	return filterByImageSet(m.imageState, m.owners, m.ownerMeta, isName)
}
