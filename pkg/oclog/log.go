package oclog

import (
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Printf writes a timestamped log line to stdout.
func Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	msg = strings.TrimSuffix(msg, "\n")
	log.Log.Info(msg)
}

// Println writes a timestamped log line to stdout.
func Println(msg string) {
	msg = strings.TrimSuffix(msg, "\n")
	log.Log.Info(msg)
}
