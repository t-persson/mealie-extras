// Command mealie-generate generates a random Mealie meal plan.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/t-persson/mealie-extras/internal/generate"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return generate.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
