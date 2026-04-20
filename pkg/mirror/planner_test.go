package mirror

import (
	"testing"
)

func TestPlanMirrorOrder_BlobReuse(t *testing.T) {
	// Simulate 5 images sharing blobs:
	//   img0: {A, B, C}        — shares A,B with img1,img2,img3,img4
	//   img1: {A, B, D}        — shares A,B
	//   img2: {A, B, E}        — shares A,B
	//   img3: {A, B, F}        — shares A,B
	//   img4: {G, H}           — unique blobs
	//
	// Optimal order: img0 first (its blobs A,B appear in most images),
	// then img1-img3 (most blobs already uploaded), img4 last (unique).

	infos := []imageBlobInfo{
		{index: 0, blobs: setOf("A", "B", "C")},
		{index: 1, blobs: setOf("A", "B", "D")},
		{index: 2, blobs: setOf("A", "B", "E")},
		{index: 3, blobs: setOf("A", "B", "F")},
		{index: 4, blobs: setOf("G", "H")},
	}

	order := greedyOrder(infos)

	// First image should be one of img0-img3 (they all share A,B with others).
	firstIdx := order[0]
	firstBlobs := infos[firstIdx].blobs
	if _, hasA := firstBlobs["A"]; !hasA {
		t.Errorf("first image should contain shared blob A, got index %d", firstIdx)
	}

	// img4 (unique blobs) should be last since it has no blobs in common.
	lastIdx := order[len(order)-1]
	if lastIdx != 4 {
		t.Errorf("last image should be index 4 (unique blobs), got %d", lastIdx)
	}

	// Verify that after the first image, each subsequent image has at least
	// one blob already uploaded (except img4 which has none in common).
	uploaded := make(map[string]struct{})
	for i, idx := range order {
		if i > 0 && idx != 4 {
			covered := 0
			for b := range infos[idx].blobs {
				if _, ok := uploaded[b]; ok {
					covered++
				}
			}
			if covered == 0 {
				t.Errorf("image at position %d (index %d) has 0 blobs already uploaded", i, idx)
			}
		}
		for b := range infos[idx].blobs {
			uploaded[b] = struct{}{}
		}
	}
}

func TestPlanMirrorOrder_IdenticalImages(t *testing.T) {
	// All images have the same blobs — after the first one, all should be instant.
	infos := []imageBlobInfo{
		{index: 0, blobs: setOf("A", "B", "C")},
		{index: 1, blobs: setOf("A", "B", "C")},
		{index: 2, blobs: setOf("A", "B", "C")},
	}

	order := greedyOrder(infos)
	if len(order) != 3 {
		t.Fatalf("expected 3 items, got %d", len(order))
	}

	// After the first image, all blobs are uploaded.
	uploaded := make(map[string]struct{})
	for b := range infos[order[0]].blobs {
		uploaded[b] = struct{}{}
	}
	for i := 1; i < len(order); i++ {
		for b := range infos[order[i]].blobs {
			if _, ok := uploaded[b]; !ok {
				t.Errorf("position %d should have all blobs covered, missing %s", i, b)
			}
		}
	}
}

func TestPlanMirrorOrder_SingleItem(t *testing.T) {
	infos := []imageBlobInfo{
		{index: 0, blobs: setOf("A")},
	}
	order := greedyOrder(infos)
	if len(order) != 1 || order[0] != 0 {
		t.Errorf("single item should return [0], got %v", order)
	}
}

func TestPlanMirrorOrder_NoBlobs(t *testing.T) {
	// Images where manifest fetch failed (empty blob sets) should still be ordered.
	infos := []imageBlobInfo{
		{index: 0, blobs: map[string]struct{}{}},
		{index: 1, blobs: setOf("A")},
		{index: 2, blobs: map[string]struct{}{}},
	}
	order := greedyOrder(infos)
	if len(order) != 3 {
		t.Fatalf("expected 3 items, got %d", len(order))
	}
	// img1 has the only blobs, so should be first (highest frequency score).
	if order[0] != 1 {
		t.Errorf("expected index 1 first (only image with blobs), got %d", order[0])
	}
}

// greedyOrder extracts just the ordering logic for unit testing without
// requiring a real registry client.
func greedyOrder(infos []imageBlobInfo) []int {
	n := len(infos)
	if n <= 1 {
		order := make([]int, n)
		for i := range infos {
			order[i] = infos[i].index
		}
		return order
	}

	blobFreq := map[string]int{}
	for _, info := range infos {
		for b := range info.blobs {
			blobFreq[b]++
		}
	}

	uploaded := map[string]struct{}{}
	remaining := make(map[int]struct{}, n)
	for i := 0; i < n; i++ {
		remaining[i] = struct{}{}
	}

	order := make([]int, 0, n)
	for len(remaining) > 0 {
		bestIdx := -1
		bestScore := -1

		for idx := range remaining {
			var score int
			if len(uploaded) == 0 {
				for b := range infos[idx].blobs {
					score += blobFreq[b]
				}
			} else {
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
		order = append(order, infos[bestIdx].index)
		delete(remaining, bestIdx)
	}

	return order
}

func setOf(keys ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}
