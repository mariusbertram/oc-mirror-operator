package oclog

import (
	"fmt"
	"time"
)

const tsFormat = "2006-01-02T15:04:05Z07:00"

func ts() string {
	return time.Now().Format(tsFormat)
}

// Printf writes a timestamped log line to stdout.
func Printf(format string, args ...any) {
	fmt.Printf("["+ts()+"] "+format, args...)
}

// Println writes a timestamped log line to stdout.
func Println(msg string) {
	fmt.Println("[" + ts() + "] " + msg)
}
