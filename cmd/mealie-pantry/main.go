// Command mealie-pantry serves a fridge, freezer and pantry tracker that
// follows a Mealie meal plan and shopping list.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/t-persson/mealie-extras/internal/pantry/app"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
