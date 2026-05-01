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
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/manager"
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
	if len(os.Args) > 1 && os.Args[1] == "resource-api" {
		runResourceAPI()
		return
	}

	var targetName, namespace string
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)

	flag.StringVar(&targetName, "mirrortarget", "", "Name of the MirrorTarget")
	flag.StringVar(&namespace, "namespace", "", "Namespace of the MirrorTarget")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}

	if targetName == "" {
		setupLog.Error(nil, "mirrortarget flag is required")
		os.Exit(1)
	}

	if namespace == "" {
		setupLog.Error(nil, "namespace flag or POD_NAMESPACE env var is required")
		os.Exit(1)
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

func runResourceAPI() {
	var namespace string
	fs := flag.NewFlagSet("resource-api", flag.ExitOnError)
	fs.StringVar(&namespace, "namespace", "", "Namespace to watch")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "flag parse error:", err)
		os.Exit(1)
	}
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create client")
		os.Exit(1)
	}
	srv := resourceapi.NewServer(c, namespace)
	srv.Run(ctrl.SetupSignalHandler())
}
