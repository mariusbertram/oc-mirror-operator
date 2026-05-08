package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	MirrorTargetImagesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "mirrortarget",
			Name:      "images_total",
			Help:      "Total number of images to be mirrored for a MirrorTarget.",
		},
		[]string{"namespace", "target"},
	)

	MirrorTargetImagesMirrored = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "mirrortarget",
			Name:      "images_mirrored",
			Help:      "Number of successfully mirrored images for a MirrorTarget.",
		},
		[]string{"namespace", "target"},
	)

	MirrorTargetImagesFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "mirrortarget",
			Name:      "images_failed",
			Help:      "Number of permanently failed images for a MirrorTarget.",
		},
		[]string{"namespace", "target"},
	)

	MirrorTargetImagesPending = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "mirrortarget",
			Name:      "images_pending",
			Help:      "Number of pending images for a MirrorTarget.",
		},
		[]string{"namespace", "target"},
	)

	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "oc_mirror",
			Name:      "reconcile_errors_total",
			Help:      "Total number of reconcile errors per controller and resource.",
		},
		[]string{"namespace", "name", "controller"},
	)

	ImageSetLastPollSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "imageset",
			Name:      "last_poll_seconds",
			Help:      "Unix timestamp of the last successful upstream poll for an ImageSet.",
		},
		[]string{"namespace", "imageset"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		MirrorTargetImagesTotal,
		MirrorTargetImagesMirrored,
		MirrorTargetImagesFailed,
		MirrorTargetImagesPending,
		ReconcileErrorsTotal,
		ImageSetLastPollSeconds,
	)
}
