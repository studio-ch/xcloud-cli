// Command xcloud is the Cloud Console command-line interface.
//
// main stays deliberately thin: signal handling and a single os.Exit
// call. Every exit code the process can produce comes from
// internal/exitcode via cmd.Execute, so the published exit-code contract
// has exactly one enforcement point.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/studio-ch/xcloud-cli/internal/cmd"
)

func main() {
	// Cancel on SIGINT/SIGTERM so in-flight requests, --wait pollers and
	// multi-part uploads can abort cleanly rather than leaving the
	// server holding a half-finished operation.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	os.Exit(cmd.Execute(ctx).Int())
}
