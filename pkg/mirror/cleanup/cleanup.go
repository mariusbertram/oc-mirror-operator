// Package cleanup deletes the mirrored images recorded in an image-state
// ConfigMap from the target registry, then removes the ConfigMap. It backs
// the "cleanup" subcommand of both the operator binary (cmd/main.go) and the
// worker binary (cmd/worker), run by the MirrorTarget controller's cleanup
// Jobs.
package cleanup

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

const (
	// DeleteTimeout bounds the deletion of one manifest.
	DeleteTimeout = 30 * time.Second
	// ClientRefreshInterval is the number of deletions after which a fresh
	// registry client is built, to avoid auth token scope overflow.
	ClientRefreshInterval = 20
)

// Deleter deletes one manifest from the registry.
// *mirrorclient.MirrorClient implements it.
type Deleter interface {
	DeleteManifest(ctx context.Context, image string) error
}

// Result counts the outcome of a cleanup run.
type Result struct {
	Deleted int
	// Skipped counts entries that were never mirrored (nothing to delete).
	Skipped int
	Failed  int
}

// Run deletes every Mirrored image recorded in the image-state ConfigMap
// cmName using registry clients from newDeleter, then deletes the ConfigMap.
// When any deletion fails, the ConfigMap is kept (so a retried Job can pick
// the remaining images up) and an error is returned alongside the counts.
func Run(ctx context.Context, c client.Client, namespace, cmName string, newDeleter func() Deleter) (Result, error) {
	var res Result
	state, err := imagestate.LoadByConfigMapName(ctx, c, namespace, cmName)
	if err != nil {
		return res, fmt.Errorf("load image state from %s: %w", cmName, err)
	}

	if len(state) == 0 {
		oclog.Printf("No images found in %s — nothing to clean up\n", cmName)
		deleteConfigMap(ctx, c, namespace, cmName)
		return res, nil
	}

	oclog.Printf("Cleaning up %d images from %s\n", len(state), cmName)

	d := newDeleter()
	count := 0
	for dest, entry := range state {
		if entry.State != "Mirrored" {
			res.Skipped++
			continue
		}
		if count > 0 && count%ClientRefreshInterval == 0 {
			d = newDeleter()
		}
		count++

		delCtx, cancel := context.WithTimeout(ctx, DeleteTimeout)
		err := d.DeleteManifest(delCtx, dest)
		cancel()
		if err != nil {
			oclog.Printf("WARN: failed to delete %s: %v", dest, err)
			res.Failed++
		} else {
			oclog.Printf("Deleted: %s\n", dest)
			res.Deleted++
		}
	}

	oclog.Printf("Cleanup complete: %d deleted, %d skipped (not mirrored), %d failed\n", res.Deleted, res.Skipped, res.Failed)
	if res.Failed > 0 {
		return res, fmt.Errorf("%d images could not be deleted", res.Failed)
	}

	deleteConfigMap(ctx, c, namespace, cmName)
	return res, nil
}

func deleteConfigMap(ctx context.Context, c client.Client, namespace, cmName string) {
	cm := &corev1.ConfigMap{}
	cm.Name = cmName
	cm.Namespace = namespace
	if err := c.Delete(ctx, cm); err != nil {
		if !k8serrors.IsNotFound(err) {
			oclog.Printf("WARN: failed to delete ConfigMap %s: %v", cmName, err)
		}
		return
	}
	oclog.Printf("Deleted ConfigMap %s\n", cmName)
}
