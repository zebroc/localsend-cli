// Package ui handles user-facing output: logging and transfer progress.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	mu sync.Mutex

	// Verbose enables Debugf output.
	Verbose bool

	// Timestamps prefixes every line with a timestamp. Enabled in daemon mode
	// where output is usually piped into a log (e.g. multilog).
	Timestamps bool
)

// Debugf logs a message only when Verbose is set.
func Debugf(format string, args ...any) {
	if Verbose {
		emit(os.Stdout, "", format, args...)
	}
}

// Infof logs a normal message to stdout.
func Infof(format string, args ...any) {
	emit(os.Stdout, "", format, args...)
}

// Warnf logs a warning to stderr.
func Warnf(format string, args ...any) {
	emit(os.Stderr, "warning: ", format, args...)
}

// Errorf logs an error to stderr.
func Errorf(format string, args ...any) {
	emit(os.Stderr, "error: ", format, args...)
}

// DebugWriter routes a log.Logger into Debugf, so noise from libraries only
// shows up in verbose mode.
type DebugWriter struct{}

func (DebugWriter) Write(p []byte) (int, error) {
	Debugf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func emit(f *os.File, prefix, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	clearLineLocked()
	if Timestamps {
		fmt.Fprint(f, time.Now().Format("2006-01-02 15:04:05")+" ")
	}
	fmt.Fprintf(f, prefix+format+"\n", args...)
}
