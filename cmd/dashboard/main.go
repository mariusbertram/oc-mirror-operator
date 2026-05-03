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

// cmd/dashboard runs the cluster-wide resource API and web dashboard.
// It is deployed as a standalone Deployment (with an oauth-proxy sidecar for
// OpenShift auth) by the DashboardReconciler.
//
// The binary supports two modes:
//
//	dashboard  (default) – serves the SPA + JSON API on :8080.
//	plugin               – serves Console Plugin static assets over HTTPS on :9001.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/resourceapi"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mirrorv1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		runPlugin()
		return
	}
	runDashboard()
}

func runDashboard() {
	var addr string
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&addr, "bind-address", ":8080", "Address the dashboard HTTP server listens on")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create cluster-wide client")
		os.Exit(1)
	}

	srv := resourceapi.NewServerClusterWide(c)

	ctx := ctrl.SetupSignalHandler()
	setupLog.Info("starting cluster-wide dashboard", "address", addr)

	// Run serves the embedded SPA and JSON API on addr.
	// We wrap it to use the custom addr instead of the default :8081.
	srv.RunOn(ctx, addr)
}

func runPlugin() {
	var addr, certFile, keyFile string
	fs := flag.NewFlagSet("plugin", flag.ExitOnError)
	fs.StringVar(&addr, "bind-address", ":9001", "Address the plugin HTTPS server listens on")
	fs.StringVar(&certFile, "cert-file", "/var/serving-cert/tls.crt", "TLS certificate file")
	fs.StringVar(&keyFile, "key-file", "/var/serving-cert/tls.key", "TLS key file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "flag parse error:", err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New())
	ctx := ctrl.SetupSignalHandler()

	mux := http.NewServeMux()
	resourceapi.RegisterPluginRoutes(mux)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	setupLog.Info("starting console plugin server", "address", addr)
	go func() {
		if err := httpSrv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "plugin server listen error")
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
