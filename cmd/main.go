/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/internal/controller"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/manager"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(mirrorv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "manager":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runManager()
			return
		case "worker":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runWorker()
			return
		case "cleanup":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runCleanup()
			return
		}
	}
	runController()
}

func runManager() {
	var targetName, namespace string
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	fs.StringVar(&targetName, "mirrortarget", "", "Name of the MirrorTarget")
	fs.StringVar(&namespace, "namespace", "", "Namespace of the MirrorTarget")
	fs.Parse(os.Args[2:])

	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}

	m, err := manager.New(targetName, namespace, scheme)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := m.Run(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func runWorker() {
	var insecure bool
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	fs.BoolVar(&insecure, "insecure", false, "Allow insecure registry")
	// --src / --dest kept for backward compatibility
	var src, dest string
	fs.StringVar(&src, "src", "", "Source image (legacy single-image mode)")
	fs.StringVar(&dest, "dest", "", "Destination image (legacy single-image mode)")
	fs.Parse(os.Args[2:])

	// Batch mode: process all images in the JSON-encoded MIRROR_BATCH env var.
	if batchJSON := os.Getenv("MIRROR_BATCH"); batchJSON != "" {
		runWorkerBatch(insecure, batchJSON)
		return
	}

	// Legacy single-image mode (used only when called without MIRROR_BATCH).
	if src == "" || dest == "" {
		fmt.Fprintln(os.Stderr, "ERROR: MIRROR_BATCH env var or --src/--dest flags are required")
		os.Exit(1)
	}
	c := buildMirrorClient(insecure, dest)
	if !mirrorOneImage(c, src, dest) {
		os.Exit(1)
	}
}

type BatchItem struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

func runWorkerBatch(insecure bool, batchJSON string) {
	var items []BatchItem
	if err := json.Unmarshal([]byte(batchJSON), &items); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to parse MIRROR_BATCH: %v\n", err)
		os.Exit(1)
	}
	if len(items) == 0 {
		fmt.Println("Empty batch, nothing to do")
		return
	}

	// Build a single client reused for all images in the batch.
	c := buildMirrorClient(insecure, items[0].Dest)

	// Compute optimal mirror order: images with the most shared blobs go first
	// so that subsequent images find those blobs via anonymous mount (zero-copy).
	sources := make([]string, len(items))
	dests := make([]string, len(items))
	for i, item := range items {
		sources[i] = item.Source
		dests[i] = item.Dest
	}
	planCtx, planCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	sources, dests = mirror.PlanMirrorOrder(planCtx, c, sources, dests)
	planCancel()

	anyFailed := false
	for i := range sources {
		// Refresh the mirror client every 20 images to prevent auth token
		// accumulation that causes "Request Header Too Large" (nginx 8KB limit).
		if i > 0 && i%20 == 0 {
			c = buildMirrorClient(insecure, dests[i])
		}
		if !mirrorOneImage(c, sources[i], dests[i]) {
			anyFailed = true
		}
	}
	// Exit 0 even if some images failed; individual failures are reported via
	// the status API so the manager can apply per-image retry logic.
	if anyFailed {
		fmt.Println("Batch completed with errors (see above)")
	}
}

func buildMirrorClient(insecure bool, firstDest string) *mirrorclient.MirrorClient {
	insecureHosts := []string{}
	destHost := ""
	if parts := strings.Split(firstDest, "/"); len(parts) > 0 {
		destHost = parts[0]
		if insecure {
			insecureHosts = append(insecureHosts, destHost)
		}
	}
	return mirrorclient.NewMirrorClient(insecureHosts, os.Getenv("DOCKER_CONFIG"), destHost)
}

// runCleanup deletes all images for a removed ImageSet from the target registry
// and removes the associated image state ConfigMap.
func runCleanup() {
	var imageSetName, namespace, registry string
	var insecure bool
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	fs.StringVar(&imageSetName, "imageset", "", "Name of the ImageSet to clean up")
	fs.StringVar(&namespace, "namespace", "", "Namespace of the ImageSet")
	fs.StringVar(&registry, "registry", "", "Target registry URL")
	fs.BoolVar(&insecure, "insecure", false, "Allow insecure registry")
	fs.Parse(os.Args[2:])

	if imageSetName == "" || namespace == "" || registry == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --imageset, --namespace, and --registry are required")
		os.Exit(1)
	}

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Load the image state for this ImageSet.
	state, err := imagestate.Load(ctx, c, namespace, imageSetName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to load image state for %s: %v\n", imageSetName, err)
		os.Exit(1)
	}

	if len(state) == 0 {
		fmt.Printf("No images found for ImageSet %s — nothing to clean up\n", imageSetName)
		deleteConfigMap(ctx, c, namespace, imageSetName)
		os.Exit(0)
	}

	fmt.Printf("Cleaning up %d images for ImageSet %s from %s\n", len(state), imageSetName, registry)

	// Build a registry client for deletion.
	mc := buildMirrorClient(insecure, registry)

	var deleted, skipped, failed int
	// Fresh client every 20 deletes to avoid auth token overflow.
	const refreshInterval = 20
	count := 0

	for dest, entry := range state {
		if entry.State != "Mirrored" {
			skipped++
			continue
		}

		if count > 0 && count%refreshInterval == 0 {
			mc = buildMirrorClient(insecure, registry)
		}
		count++

		delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := mc.DeleteManifest(delCtx, dest); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: failed to delete %s: %v\n", dest, err)
			failed++
		} else {
			fmt.Printf("Deleted: %s\n", dest)
			deleted++
		}
		cancel()
	}

	fmt.Printf("Cleanup complete: %d deleted, %d skipped (not mirrored), %d failed\n", deleted, skipped, failed)

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d images could not be deleted\n", failed)
		os.Exit(1)
	}

	// All images deleted successfully — remove the ConfigMap.
	deleteConfigMap(ctx, c, namespace, imageSetName)
}

func deleteConfigMap(ctx context.Context, c client.Client, namespace, imageSetName string) {
	cmName := imagestate.ConfigMapName(imageSetName)
	cm := &corev1.ConfigMap{}
	cm.Name = cmName
	cm.Namespace = namespace
	if err := c.Delete(ctx, cm); err != nil {
		if !k8serrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "WARN: failed to delete ConfigMap %s: %v\n", cmName, err)
		}
	} else {
		fmt.Printf("Deleted ConfigMap %s\n", cmName)
	}
}

// mirrorOneImage mirrors src→dest with a single retry for transient errors and
// reports the result to the manager via the status API. Returns true on success.
// The manager handles higher-level retry orchestration (Failed→Pending requeue).
func mirrorOneImage(c *mirrorclient.MirrorClient, src, dest string) bool {
	fmt.Printf("Starting mirror: %s -> %s\n", src, dest)

	// Per-image timeout prevents indefinite hangs on stalled blob uploads.
	const imageTimeout = 20 * time.Minute

	var effectiveDest string
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			fmt.Printf("Retry attempt 2/2 after 15s...\n")
			time.Sleep(15 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), imageTimeout)
		effectiveDest, lastErr = c.CopyImage(ctx, src, dest)
		cancel()
		if lastErr == nil {
			break
		}
		fmt.Printf("Attempt %d failed: %v\n", attempt, lastErr)
	}
	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to mirror %s: %v\n", src, lastErr)
		setupLog.Error(lastErr, "failed to mirror image")
		reportStatus(dest, "", lastErr.Error())
		return false
	}

	fmt.Printf("Copy complete, verifying digest at %s\n", effectiveDest)
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	digest, err := c.GetDigest(verifyCtx, effectiveDest)
	verifyCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to verify digest for %s: %v\n", src, err)
		setupLog.Error(err, "failed to verify mirrored image digest")
		reportStatus(dest, "", err.Error())
		return false
	}

	fmt.Printf("Successfully mirrored %s -> %s (digest: %s)\n", src, dest, digest)
	setupLog.Info("successfully mirrored image", "src", src, "dest", dest, "digest", digest)
	reportStatus(dest, digest, "")
	return true
}

type WorkerStatusRequest struct {
	PodName     string `json:"podName"`
	Destination string `json:"destination"`
	Digest      string `json:"digest"`
	Error       string `json:"error,omitempty"`
}

func reportStatus(dest, digest, errMsg string) {
	managerURL := os.Getenv("MANAGER_URL")
	podName := os.Getenv("POD_NAME")
	if managerURL == "" || podName == "" {
		return
	}

	req := WorkerStatusRequest{
		PodName:     podName,
		Destination: dest,
		Digest:      digest,
		Error:       errMsg,
	}

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", managerURL+"/status", bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("Failed to build status request: %v\n", err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+os.Getenv("WORKER_TOKEN"))
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		fmt.Printf("Failed to report status to manager: %v\n", err)
		return
	}
	defer resp.Body.Close()
}

func runController() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	syncPeriod := 1 * time.Hour
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "143c4491.mirror.openshift.io",
		// SyncPeriod ensures all watched resources are periodically re-reconciled.
		// This makes poll-based image collection durable across operator restarts —
		// the actual poll decision is gated on LastSuccessfulPollTime in the ImageSet status.
		Cache: cache.Options{SyncPeriod: &syncPeriod},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.MirrorTargetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MirrorTarget")
		os.Exit(1)
	}
	if err := (&controller.ImageSetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageSet")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
