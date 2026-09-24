package worker

import (
	"context"
	"flag"
	"os"

	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

// Main runs the worker with the given command-line arguments (without the
// program or subcommand name) and returns the process exit code.
//
// Batch mode processes the JSON array in the MIRROR_BATCH env var; the
// legacy single-image mode takes --src/--dest.
func Main(args []string) int {
	var insecure bool
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.BoolVar(&insecure, "insecure", false, "Allow insecure registry")
	// --src / --dest kept for backward compatibility
	var src, dest string
	fs.StringVar(&src, "src", "", "Source image (legacy single-image mode)")
	fs.StringVar(&dest, "dest", "", "Destination image (legacy single-image mode)")
	if err := fs.Parse(args); err != nil {
		oclog.Printf("flag parse error: %v", err)
		return 1
	}
	return run(context.Background(), New(insecure), os.Getenv("MIRROR_BATCH"), src, dest)
}

func run(ctx context.Context, w *Worker, batchJSON, src, dest string) int {
	if batchJSON != "" {
		items, err := ParseBatch(batchJSON)
		if err != nil {
			oclog.Printf("ERROR: %v", err)
			return 1
		}
		// Exit 0 even if some images failed; individual failures are
		// reported via the status API.
		w.RunBatch(ctx, items)
		return 0
	}

	if src == "" || dest == "" {
		oclog.Println("ERROR: MIRROR_BATCH env var or --src/--dest flags are required")
		return 1
	}
	if !w.MirrorOne(ctx, w.NewClient(dest), src, dest) {
		return 1
	}
	return 0
}
