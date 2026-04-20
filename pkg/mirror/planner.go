package mirror

import (
	"context"
	"fmt"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
)

// imageBlobInfo maps a batch item index to its set of blob digests.
type imageBlobInfo struct {
	index int
	blobs map[string]struct{}
}

// PlanMirrorOrder inspects source manifests and returns items reordered so that
// images sharing the most blobs with earlier images come later. This maximises
// hits on regclient's anonymous-blob-mount fast-path: blobs pushed by an
// earlier image are found in Quay's global storage and linked into the next
// repository with zero data transfer.
//
// The algorithm is a greedy set-cover:
//  1. Pick the image whose blobs appear in the most other images (seeds shared layers early).
//  2. Pick the image with the most blobs already covered (minimises new uploads).
//  3. Repeat until all images are scheduled.
func PlanMirrorOrder(ctx context.Context, client *mirrorclient.MirrorClient, sources, dests []string) ([]string, []string) {
	n := len(sources)
	if n <= 1 {
		return sources, dests
	}

	// Phase 1: fetch manifests and collect blob digests per image.
	infos := make([]imageBlobInfo, n)
	for i := 0; i < n; i++ {
		blobs, err := extractBlobDigests(ctx, client, sources[i])
		if err != nil {
			fmt.Printf("Planner: could not inspect %s: %v\n", sources[i], err)
			blobs = map[string]struct{}{}
		}
		infos[i] = imageBlobInfo{index: i, blobs: blobs}
	}

	// Phase 2: count how many images reference each blob (frequency).
	blobFreq := map[string]int{}
	for _, info := range infos {
		for b := range info.blobs {
			blobFreq[b]++
		}
	}

	// Phase 3: greedy ordering.
	uploaded := map[string]struct{}{}
	remaining := make(map[int]struct{}, n)
	for i := 0; i < n; i++ {
		remaining[i] = struct{}{}
	}

	orderedSrc := make([]string, 0, n)
	orderedDst := make([]string, 0, n)

	for len(remaining) > 0 {
		bestIdx := -1
		bestScore := -1

		for idx := range remaining {
			var score int
			if len(uploaded) == 0 {
				// First pick: choose image whose blobs appear in the most other
				// images — uploading these blobs first maximises future savings.
				for b := range infos[idx].blobs {
					score += blobFreq[b]
				}
			} else {
				// Subsequent picks: prefer the image with the most blobs already
				// uploaded (= most anonymous-mount hits, fewest new uploads).
				for b := range infos[idx].blobs {
					if _, ok := uploaded[b]; ok {
						score++
					}
				}
			}

			if score > bestScore || (score == bestScore && bestIdx == -1) {
				bestScore = score
				bestIdx = idx
			}
		}

		for b := range infos[bestIdx].blobs {
			uploaded[b] = struct{}{}
		}
		orderedSrc = append(orderedSrc, sources[bestIdx])
		orderedDst = append(orderedDst, dests[bestIdx])
		delete(remaining, bestIdx)
	}

	fmt.Printf("Planner: ordered %d images, %d unique blobs across batch\n", n, len(uploaded))
	return orderedSrc, orderedDst
}

// extractBlobDigests fetches the manifest for src and returns all blob digests
// (layers + config) referenced by the image. For multi-arch images it resolves
// every platform manifest.
func extractBlobDigests(ctx context.Context, client *mirrorclient.MirrorClient, src string) (map[string]struct{}, error) {
	srcRef, err := ref.New(src)
	if err != nil {
		return nil, err
	}

	m, err := client.ManifestGet(ctx, srcRef)
	if err != nil {
		return nil, err
	}

	blobs := map[string]struct{}{}

	if m.IsList() {
		descs, err := m.GetManifestList()
		if err != nil {
			return blobs, nil
		}
		for _, d := range descs {
			if dig := d.Digest.String(); dig != "" {
				blobs[dig] = struct{}{}
			}
			// Resolve each platform manifest to get its layers.
			platRef := srcRef
			platRef.Tag = ""
			platRef.Digest = d.Digest.String()
			platM, err := client.ManifestGet(ctx, platRef)
			if err != nil {
				continue
			}
			collectBlobs(platM, blobs)
		}
	} else {
		collectBlobs(m, blobs)
	}

	return blobs, nil
}

// collectBlobs extracts config and layer digests from a single-platform manifest.
func collectBlobs(m manifest.Manifest, blobs map[string]struct{}) {
	cd, err := m.GetConfig()
	if err == nil && cd.Digest.String() != "" {
		blobs[cd.Digest.String()] = struct{}{}
	}

	layers, err := m.GetLayers()
	if err == nil {
		for _, l := range layers {
			if dig := l.Digest.String(); dig != "" {
				blobs[dig] = struct{}{}
			}
		}
	}
}
