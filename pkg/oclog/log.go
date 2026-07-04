package oclog

import (
	"fmt"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("oclog")

// Printf writes a log line using controller-runtime's structured logger.
func Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Info(strings.TrimSuffix(msg, "\n"))
}

// Println writes a log line using controller-runtime's structured logger.
func Println(msg string) {
	log.Info(strings.TrimSuffix(msg, "\n"))
}
