package freezer

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

type fake struct {
	plans         []api.MealPlan
	start, end    string
	title         string
	message       string
	click         string
	notifyErr     error
	plansErr      error
	notifications int
}

func (f *fake) MealPlans(_ context.Context, start, end string) ([]api.MealPlan, error) {
	f.start, f.end = start, end
	return f.plans, f.plansErr
}

func (f *fake) Notify(_ context.Context, title, message, click string) error {
	f.notifications++
	f.title, f.message, f.click = title, message, click
	return f.notifyErr
}

func run(t *testing.T, now time.Time, f *fake) error {
	t.Helper()
	var output bytes.Buffer
	return Execute(context.Background(), now, f, cli.NewLogger(&output, "INFO", true))
}

func TestReminderUsesTomorrowAndListsMeals(t *testing.T) {
	f := &fake{plans: []api.MealPlan{
		{Recipe: api.MealPlanRecipe{Name: "Soup"}},
		{Recipe: api.MealPlanRecipe{Name: "Bread"}},
	}}
	if err := run(t, time.Date(2026, 11, 17, 16, 0, 0, 0, time.FixedZone("CET", 3600)), f); err != nil {
		t.Fatal(err)
	}
	if f.start != "2026-11-18" || f.end != f.start {
		t.Fatalf("date range = %q to %q", f.start, f.end)
	}
	if f.title != ntfyTitle || f.message != "Tomorrow's meal: Soup, Bread.\n\nRemember to retrieve the ingredients from the freezer." ||
		f.click != ntfyClick {
		t.Fatalf("notification = %q %q %q", f.title, f.message, f.click)
	}
}

func TestReminderNotifiesWhenNoMealIsPlanned(t *testing.T) {
	f := &fake{}
	if err := run(t, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), f); err != nil {
		t.Fatal(err)
	}
	if f.message != "No meal is planned for tomorrow.\n\nRemember to retrieve the ingredients from the freezer." {
		t.Fatalf("message = %q", f.message)
	}
}

func TestReminderFailuresSkipNotification(t *testing.T) {
	boom := errors.New("boom")
	for name, f := range map[string]*fake{
		"query":  {plansErr: boom},
		"notify": {notifyErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t, time.Now(), f)
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v", err)
			}
			if name == "query" && f.notifications != 0 {
				t.Fatal("notified after query failure")
			}
		})
	}
}

func TestReminderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fake{}
	var output bytes.Buffer
	cancel()
	err := Execute(ctx, time.Now(), f, cli.NewLogger(&output, "INFO", true))
	if !errors.Is(err, context.Canceled) || f.notifications != 0 {
		t.Fatalf("error = %v, notifications = %d", err, f.notifications)
	}
}
