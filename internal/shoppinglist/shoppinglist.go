// Package shoppinglist implements the mealie-shoppinglist command workflow.
package shoppinglist

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

const (
	dateLayout  = "2006-01-02"
	ntfyTitle   = "Veckohandling has been updated"
	ntfyMessage = "Please uncheck items that are already in stock"
)

// Service is the set of backend operations the workflow needs.
type Service interface {
	MealPlans(ctx context.Context, startDate, endDate string) ([]api.MealPlan, error)
	Recipe(ctx context.Context, slug string) (api.Recipe, error)
	AddRecipes(ctx context.Context, listID string, recipes []api.Recipe) error
	Notify(ctx context.Context, title, message, click string) error
}

// Run parses args and executes the command, returning a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := cli.Parse("mealie-shoppinglist", args, time.Now(), stdout)
	if err != nil {
		if err == cli.ErrHelp {
			return 0
		}
		fmt.Fprintf(stderr, "mealie-shoppinglist: %v\n", err)
		return 2
	}
	logger := cli.NewLogger(stdout, options.LogLevel, options.CleanText)
	if err := Execute(ctx, options, api.NewClient(options.APIKey), logger); err != nil {
		fmt.Fprintf(stderr, "mealie-shoppinglist: %v\n", err)
		return 1
	}
	return 0
}

// Execute runs the shopping-list workflow.
func Execute(ctx context.Context, options cli.Options, service Service, logger *cli.Logger) error {
	start := options.StartDate.Format(dateLayout)
	end := options.StartDate.AddDate(0, 0, options.Num-1).Format(dateLayout)
	numText := options.NumText
	if numText == "" {
		numText = strconv.Itoa(options.Num)
	}
	logger.Info("Create shopping list for '%s' meals starting with '%s'", numText, start)

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("query meal plans: %w", err)
	}
	plans, err := service.MealPlans(ctx, start, end)
	if err != nil {
		return fmt.Errorf("query meal plans %s to %s: %w", start, end, err)
	}
	recipes := make([]api.Recipe, 0, len(plans))
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("look up recipe %q: %w", plan.Recipe.Slug, err)
		}
		recipe, err := service.Recipe(ctx, plan.Recipe.Slug)
		if err != nil {
			return fmt.Errorf("look up recipe %q: %w", plan.Recipe.Slug, err)
		}
		recipes = append(recipes, recipe)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("add recipes to shopping list %s: %w", options.ListID, err)
	}
	if err := service.AddRecipes(ctx, options.ListID, recipes); err != nil {
		return fmt.Errorf("add recipes to shopping list %s: %w", options.ListID, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	click := "https://mealie.tp-softworks.se" + "/shopping-lists/" + options.ListID
	if err := service.Notify(ctx, ntfyTitle, ntfyMessage, click); err != nil {
		// Skip errors for now, since notify is not critical to the workflow.
		// return fmt.Errorf("send notification: %w", err)
		logger.Info("send notification: %s", err.Error())
	}
	return nil
}
