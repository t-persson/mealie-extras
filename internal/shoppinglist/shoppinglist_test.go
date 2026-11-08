package shoppinglist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

type fake struct {
	plans              []api.MealPlan
	start, end         string
	looked             []string
	added              []api.Recipe
	addCalled          bool
	listID             string
	title, msg, click  string
	notified           bool
	planErr, recipeErr error
	addErr, notifyErr  error
	onPlans            func()
}

func (f *fake) MealPlans(_ context.Context, s, e string) ([]api.MealPlan, error) {
	f.start, f.end = s, e
	if f.onPlans != nil {
		f.onPlans()
	}
	return f.plans, f.planErr
}
func (f *fake) Recipe(_ context.Context, slug string) (api.Recipe, error) {
	f.looked = append(f.looked, slug)
	return api.Recipe{ID: "id-" + slug}, f.recipeErr
}
func (f *fake) AddRecipes(_ context.Context, id string, r []api.Recipe) error {
	f.addCalled, f.listID, f.added = true, id, r
	return f.addErr
}
func (f *fake) Notify(_ context.Context, t, m, c string) error {
	f.notified, f.title, f.msg, f.click = true, t, m, c
	return f.notifyErr
}

func plan(slug string) api.MealPlan {
	var p api.MealPlan
	p.Recipe.Slug = slug
	return p
}

func opts(num int) cli.Options {
	return cli.Options{StartDate: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Num: num, NumText: fmt.Sprint(num), ListID: "list-1"}
}

func run(ctx context.Context, o cli.Options, f *fake) (string, error) {
	var buf bytes.Buffer
	err := Execute(ctx, o, f, cli.NewLogger(&buf, "INFO", true))
	return buf.String(), err
}

func TestWorkflowRangeRepeatedRecipeAndNotify(t *testing.T) {
	f := &fake{plans: []api.MealPlan{plan("a"), plan("b"), plan("a")}}
	out, err := run(context.Background(), opts(7), f)
	if err != nil {
		t.Fatal(err)
	}
	if f.start != "2026-11-09" || f.end != "2026-11-15" {
		t.Fatalf("range %s %s", f.start, f.end)
	}
	if !reflect.DeepEqual(f.looked, []string{"a", "b", "a"}) || len(f.added) != 3 || f.listID != "list-1" {
		t.Fatalf("looked %v added %v list %s", f.looked, f.added, f.listID)
	}
	if f.title != "Veckohandling has been updated" || f.msg != "Please uncheck items that are already in stock" ||
		f.click != "https://mealie.tp-softworks.se/shopping-lists/list-1" {
		t.Fatalf("notify %q %q %q", f.title, f.msg, f.click)
	}
	if !strings.Contains(out, "Create shopping list for '7' meals starting with '2026-11-09'") {
		t.Fatalf("log %q", out)
	}
}

func TestZeroNumAndEmptyPlan(t *testing.T) {
	f := &fake{}
	if _, err := run(context.Background(), opts(0), f); err != nil {
		t.Fatal(err)
	}
	if f.end != "2026-11-08" || !f.addCalled || len(f.added) != 0 || !f.notified {
		t.Fatalf("%+v", f)
	}
}

func TestDebugLogsPreservePythonOutput(t *testing.T) {
	f := &fake{plans: []api.MealPlan{plan("a")}}
	var output bytes.Buffer
	options := opts(2)
	options.NumText = "02"
	if err := Execute(context.Background(), options, f, cli.NewLogger(&output, "DEBUG", true)); err != nil {
		t.Fatal(err)
	}
	if want := "Create shopping list for '02' meals starting with '2026-11-09'\n"; output.String() != want {
		t.Fatalf("logs = %q, want %q", output.String(), want)
	}
}

func TestFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		f        *fake
		wantAdd  bool
		wantNote bool
	}{
		"query":  {&fake{planErr: boom}, false, false},
		"lookup": {&fake{plans: []api.MealPlan{plan("a")}, recipeErr: boom}, false, false},
		"add":    {&fake{addErr: boom}, true, false},
		"notify": {&fake{notifyErr: boom}, true, true},
	}
	for name, c := range cases {
		_, err := run(context.Background(), opts(7), c.f)
		if !errors.Is(err, boom) {
			t.Errorf("%s: err %v", name, err)
		}
		if c.f.addCalled != c.wantAdd || c.f.notified != c.wantNote {
			t.Errorf("%s: add %v notified %v", name, c.f.addCalled, c.f.notified)
		}
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fake{plans: []api.MealPlan{plan("a")}, onPlans: cancel}
	_, err := run(ctx, opts(7), f)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(f.looked) != 0 || f.addCalled || f.notified {
		t.Fatalf("work continued: %+v", f)
	}
}
