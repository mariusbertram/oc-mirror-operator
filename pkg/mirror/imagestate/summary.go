package imagestate

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Summary holds the MirrorTarget-level image counts, deduplicated by
// destination across all of the target's ImageSets (see Counts), together
// with the ImageSets they were computed over. The manager, which holds the
// merged state in memory anyway, writes it on every state flush so the
// MirrorTarget controller can publish its totals without decoding every
// ImageSet's state ConfigMap.
type Summary struct {
	// ImageSets is the sorted list of ImageSet names the counts cover.
	ImageSets []string
	Total     int
	Mirrored  int
	Pending   int
	Failed    int
}

const (
	summaryKeyImageSets = "imageSets"
	summaryKeyTotal     = "total"
	summaryKeyMirrored  = "mirrored"
	summaryKeyPending   = "pending"
	summaryKeyFailed    = "failed"
)

// SummaryConfigMapName returns the name of a MirrorTarget's summary
// ConfigMap.
func SummaryConfigMapName(mtName string) string {
	return mtName + "-images-summary"
}

func (s Summary) data() map[string]string {
	return map[string]string{
		summaryKeyImageSets: strings.Join(s.ImageSets, ","),
		summaryKeyTotal:     strconv.Itoa(s.Total),
		summaryKeyMirrored:  strconv.Itoa(s.Mirrored),
		summaryKeyPending:   strconv.Itoa(s.Pending),
		summaryKeyFailed:    strconv.Itoa(s.Failed),
	}
}

// SaveSummary writes the summary ConfigMap for mtName, skipping the write
// when its content is unchanged. If owner and scheme are provided, a
// ControllerReference is set on a newly created ConfigMap.
func SaveSummary(ctx context.Context, c client.Client, namespace, mtName string, s Summary, owner metav1.Object, scheme *runtime.Scheme) error {
	cmName := SummaryConfigMapName(mtName)
	data := s.data()

	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("get summary configmap: %w", err)
	}
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: namespace},
			Data:       data,
		}
		if owner != nil && scheme != nil {
			if err := controllerutil.SetControllerReference(owner, cm, scheme); err != nil {
				return fmt.Errorf("set controller reference: %w", err)
			}
		}
		return c.Create(ctx, cm)
	}
	if maps.Equal(existing.Data, data) {
		return nil
	}
	existing.Data = data
	return c.Update(ctx, existing)
}

// LoadSummary reads the summary ConfigMap for mtName. ok is false when it
// does not exist yet or cannot be parsed (e.g. written by a newer version);
// callers then fall back to computing the counts themselves.
func LoadSummary(ctx context.Context, c client.Client, namespace, mtName string) (s Summary, ok bool, err error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: SummaryConfigMapName(mtName)}, cm); err != nil {
		if errors.IsNotFound(err) {
			return Summary{}, false, nil
		}
		return Summary{}, false, fmt.Errorf("get summary configmap: %w", err)
	}
	if raw, found := cm.Data[summaryKeyImageSets]; found && raw != "" {
		s.ImageSets = strings.Split(raw, ",")
	}
	for key, dst := range map[string]*int{
		summaryKeyTotal:    &s.Total,
		summaryKeyMirrored: &s.Mirrored,
		summaryKeyPending:  &s.Pending,
		summaryKeyFailed:   &s.Failed,
	} {
		v, convErr := strconv.Atoi(cm.Data[key])
		if convErr != nil {
			return Summary{}, false, nil
		}
		*dst = v
	}
	return s, true, nil
}
