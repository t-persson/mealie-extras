// Package freezer implements the mealie-freezer reminder workflow.
package freezer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

const (
	dateLayout = "2006-01-02"
	ntfyTitle  = "Meal planned for tomorrow"
	ntfyClick  = "https://mealie.tp-softworks.se/household/mealplan/planner/view"
	mealieURL  = "https://mealie.tp-softworks.se"
	command    = "mealie-freezer"
)

// Service is the set of backend operations used by the reminder.
type Service interface {
	MealPlans(ctx context.Context, startDate, endDate string) ([]api.MealPlan, error)
	NotifyWithAttachment(ctx context.Context, title, message, click, attach string) error
}

// Run parses args and executes the command, returning a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	now := time.Now()
	options, err := cli.Parse(command, args, now, stdout)
	if err != nil {
		if errors.Is(err, cli.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 2
	}
	logger := cli.NewLogger(stdout, options.LogLevel, options.CleanText)
	if err := Execute(ctx, now, api.NewClientWithTokens(options.APIKey, options.NtfyToken), logger); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1
	}
	return 0
}

// Execute sends a reminder containing the meals planned for the day after now.
func Execute(ctx context.Context, now time.Time, service Service, logger *cli.Logger) error {
	tomorrow := now.In(now.Location()).AddDate(0, 0, 1)
	date := tomorrow.Format(dateLayout)
	logger.Info("Find meal planned for tomorrow on '%s'", date)

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("query meal plans: %w", err)
	}
	plans, err := service.MealPlans(ctx, date, date)
	if err != nil {
		return fmt.Errorf("query meal plans for %s: %w", date, err)
	}

	names := make([]string, 0, len(plans))
	for _, plan := range plans {
		if plan.Recipe.Name != "" {
			names = append(names, plan.Recipe.Name)
		}
	}
	message := "No meal is planned for tomorrow."
	if len(names) > 0 {
		message = "Tomorrow's meal: " + strings.Join(names, ", ") + "."
	}
	message += "\n\nRemember to retrieve the ingredients from the freezer."

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	click, attach := ntfyClick, ""
	if len(plans) == 1 && plans[0].Recipe.Slug != "" {
		slug := url.PathEscape(plans[0].Recipe.Slug)
		click = mealieURL + "/recipe/" + slug
		if plans[0].RecipeID != "" {
			attach = mealieURL + "/api/media/recipes/" + url.PathEscape(plans[0].RecipeID) + "/images/min-original.webp"
		}
	}
	if err := service.NotifyWithAttachment(ctx, ntfyTitle, message, click, attach); err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	return nil
}
