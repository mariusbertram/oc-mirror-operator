package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ManagerBatchesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "batches_total",
			Help:      "Total number of worker batches dispatched.",
		},
		[]string{"target", "result"},
	)

	ManagerImagesMirroredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "images_mirrored_total",
			Help:      "Total number of images successfully mirrored.",
		},
		[]string{"target", "imageset"},
	)

	ManagerImagesFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "images_failed_total",
			Help:      "Total number of images that failed to mirror.",
		},
		[]string{"target", "imageset"},
	)

	ManagerBatchDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "batch_duration_seconds",
			Help:      "Duration of worker batch execution in seconds.",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 12), // 1s…4096s
		},
		[]string{"target"},
	)

	ManagerActiveWorkers = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "active_workers",
			Help:      "Number of currently active worker pods.",
		},
		[]string{"target"},
	)

	ManagerWorkerRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "oc_mirror",
			Subsystem: "manager",
			Name:      "worker_retries_total",
			Help:      "Total number of image retry attempts.",
		},
		[]string{"target"},
	)
)

// ManagerRegistry is a dedicated prometheus registry for manager-pod metrics.
// Using a separate registry avoids conflicts with the controller-runtime default
// registry when both processes are compiled into the same binary.
var ManagerRegistry = prometheus.NewRegistry()

func init() {
	ManagerRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		ManagerBatchesTotal,
		ManagerImagesMirroredTotal,
		ManagerImagesFailedTotal,
		ManagerBatchDurationSeconds,
		ManagerActiveWorkers,
		ManagerWorkerRetriesTotal,
	)
}

// NewManagerMetricsHandler returns an HTTP handler that serves manager metrics.
func NewManagerMetricsHandler() http.Handler {
	return promhttp.HandlerFor(ManagerRegistry, promhttp.HandlerOpts{})
}
