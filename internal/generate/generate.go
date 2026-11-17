// Package generate implements the mealie-generate workflow.
package generate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

const (
	dateLayout   = "2006-01-02"
	notifyTitle  = "A new meal plan has been generated"
	notifyClick  = "https://mealie.tp-softworks.se/household/mealplan/planner/view"
	commandName  = "mealie-generate"
	exitOK       = 0
	exitFailure  = 1
	exitUsageErr = 2
)

// Service is the subset of the Mealie/ntfy API used by Execute.
type Service interface {
	MealPlans(ctx context.Context, startDate, endDate string) ([]api.MealPlan, error)
	RandomMealPlan(ctx context.Context, date string) (api.MealPlan, error)
	DeleteMealPlan(ctx context.Context, id int) error
	Notify(ctx context.Context, title, message, click string) error
}

// Run parses args, runs the workflow and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := cli.Parse(commandName, args, time.Now(), stdout)
	if err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return exitUsageErr
	}
	logger := cli.NewLogger(stdout, options.LogLevel, options.CleanText)
	if err := Execute(ctx, options, api.NewClientWithTokens(options.APIKey, options.NtfyToken), logger); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return exitFailure
	}
	return exitOK
}

// quote mimics Python's repr for simple strings.
func quote(s string) string { return "'" + s + "'" }

// Execute generates options.Num meals starting at options.StartDate, avoiding
// recipes already planned, then sends a notification.
func Execute(ctx context.Context, options cli.Options, service Service, logger *cli.Logger) error {
	start := options.StartDate
	startStr := start.Format(dateLayout)
	numText := options.NumText
	if numText == "" {
		numText = strconv.Itoa(options.Num)
	}
	logger.Info("Start generate %s meals starting with %s", quote(numText), quote(startStr))

	existing, err := service.MealPlans(ctx, "", "")
	if err != nil {
		return fmt.Errorf("list meal plans: %w", err)
	}
	recipeIDs := make(map[string]struct{}, len(existing)+options.Num)
	for _, plan := range existing {
		recipeIDs[plan.RecipeID] = struct{}{}
	}

	current := start
	target := startStr
	for i := 0; i < options.Num; i++ {
		logger.Info("Adding a new meal for %s", quote(target))
		plan, err := randomPlan(ctx, service, target)
		if err != nil {
			return err
		}
		for {
			if _, seen := recipeIDs[plan.RecipeID]; !seen {
				break
			}
			logger.Info("Recipe %s has already been made the last 14 days", quote(plan.RecipeID))
			if err := service.DeleteMealPlan(ctx, plan.ID); err != nil {
				return fmt.Errorf("delete meal plan %d: %w", plan.ID, err)
			}
			if plan, err = randomPlan(ctx, service, target); err != nil {
				return err
			}
		}
		logger.Info("Recipe added to meal plan: %s", quote(plan.RecipeID))
		recipeIDs[plan.RecipeID] = struct{}{}
		current = current.AddDate(0, 0, 1)
		target = current.Format(dateLayout)
	}

	end := current.AddDate(0, 0, -1).Format(dateLayout)
	message := fmt.Sprintf("A new mealplan starting on %s and ending %s has been generated", startStr, end)
	if err := service.Notify(ctx, notifyTitle, message, notifyClick); err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	return nil
}

func randomPlan(ctx context.Context, service Service, date string) (api.MealPlan, error) {
	if err := ctx.Err(); err != nil {
		return api.MealPlan{}, fmt.Errorf("create random meal plan for %s: %w", date, err)
	}
	plan, err := service.RandomMealPlan(ctx, date)
	if err != nil {
		return api.MealPlan{}, fmt.Errorf("create random meal plan for %s: %w", date, err)
	}
	return plan, nil
}
