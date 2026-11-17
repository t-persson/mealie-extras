// Command mealie-freezer reminds the household to retrieve tomorrow's meal ingredients.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/t-persson/mealie-extras/internal/freezer"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return freezer.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
