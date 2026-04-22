package state

import (
	"context"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

func TestStateCoverage(t *testing.T) {
	mc := client.NewMirrorClient(nil, "")
	sm := New(mc)

	meta := &Metadata{
		MirroredImages: map[string]string{"a": "b"},
	}

	_, _ = sm.WriteMetadata(context.TODO(), "repo", "tag", meta)
	_, _, _ = sm.ReadMetadata(context.TODO(), "repo", "tag")
}
