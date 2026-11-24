// Command cluster-connection-reference serves cluster-connection v1alpha1 from memory, and runs the
// fixture subcommands of its conformance suites against a running one.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := reference.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
