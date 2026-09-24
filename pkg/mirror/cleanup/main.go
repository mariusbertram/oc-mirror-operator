package cleanup

import (
	"context"
	"flag"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/worker"
	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

// Options are the cleanup subcommand's flags.
type Options struct {
	ImageSet  string
	Namespace string
	Registry  string
	Insecure  bool
	// ConfigMap overrides the ConfigMap name derived from ImageSet.
	ConfigMap string
}

// ParseFlags parses the cleanup subcommand's arguments (without the program
// or subcommand name) and validates them.
func ParseFlags(args []string) (Options, bool) {
	var o Options
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.StringVar(&o.ImageSet, "imageset", "", "Name of the ImageSet to clean up")
	fs.StringVar(&o.Namespace, "namespace", "", "Namespace of the ImageSet")
	fs.StringVar(&o.Registry, "registry", "", "Target registry URL")
	fs.BoolVar(&o.Insecure, "insecure", false, "Allow insecure registry")
	fs.StringVar(&o.ConfigMap, "configmap", "", "Override ConfigMap name (default: derived from --imageset)")
	if err := fs.Parse(args); err != nil {
		oclog.Printf("flag parse error: %v", err)
		return o, false
	}
	if o.Namespace == "" || o.Registry == "" {
		oclog.Println("ERROR: --namespace and --registry are required")
		return o, false
	}
	if o.ImageSet == "" && o.ConfigMap == "" {
		oclog.Println("ERROR: --imageset or --configmap is required")
		return o, false
	}
	if o.ConfigMap == "" {
		o.ConfigMap = imagestate.ConfigMapName(o.ImageSet)
	}
	return o, true
}

// Main runs the cleanup subcommand with the given arguments (without the
// program or subcommand name) against the cluster from the default
// kubeconfig/in-cluster config, and returns the process exit code.
func Main(args []string, scheme *runtime.Scheme) int {
	o, ok := ParseFlags(args)
	if !ok {
		return 1
	}
	cfg, err := config.GetConfig()
	if err != nil {
		oclog.Printf("ERROR: failed to load Kubernetes config: %v", err)
		return 1
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		oclog.Printf("ERROR: failed to create Kubernetes client: %v", err)
		return 1
	}
	return run(context.Background(), c, o)
}

func run(ctx context.Context, c client.Client, o Options) int {
	oclog.Printf("Cleanup target registry: %s\n", o.Registry)
	newDeleter := func() Deleter { return worker.NewMirrorClient(o.Insecure, o.Registry) }
	if _, err := Run(ctx, c, o.Namespace, o.ConfigMap, newDeleter); err != nil {
		oclog.Printf("ERROR: %v", err)
		return 1
	}
	return 0
}
