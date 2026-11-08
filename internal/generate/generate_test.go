package generate

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
)

type notification struct{ title, message, click string }

type fake struct {
	initial   []api.MealPlan
	randoms   []api.MealPlan
	calls     []string
	notes     []notification
	listErr   error
	randErr   error
	deleteErr error
	notifyErr error
	onRandom  func()
}

func (f *fake) MealPlans(_ context.Context, s, e string) ([]api.MealPlan, error) {
	f.calls = append(f.calls, "list")
	return f.initial, f.listErr
}

func (f *fake) RandomMealPlan(_ context.Context, date string) (api.MealPlan, error) {
	f.calls = append(f.calls, "random "+date)
	if f.onRandom != nil {
		f.onRandom()
	}
	if f.randErr != nil {
		return api.MealPlan{}, f.randErr
	}
	p := f.randoms[0]
	f.randoms = f.randoms[1:]
	return p, nil
}

func (f *fake) DeleteMealPlan(_ context.Context, id int) error {
	f.calls = append(f.calls, "delete "+string(rune('0'+id)))
	return f.deleteErr
}

func (f *fake) Notify(_ context.Context, t, m, c string) error {
	f.calls = append(f.calls, "notify")
	f.notes = append(f.notes, notification{t, m, c})
	return f.notifyErr
}

func opts(num int) cli.Options {
	return cli.Options{
		StartDate: time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		Num:       num,
		LogLevel:  "INFO",
		CleanText: true,
	}
}

func plan(id int, recipe string) api.MealPlan { return api.MealPlan{ID: id, RecipeID: recipe} }

func run(t *testing.T, o cli.Options, f *fake, ctx context.Context) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Execute(ctx, o, f, cli.NewLogger(&out, o.LogLevel, o.CleanText))
	return out.String(), err
}

func TestWorkflowDuplicateAndDateAdvance(t *testing.T) {
	f := &fake{
		initial: []api.MealPlan{plan(9, "old")},
		randoms: []api.MealPlan{plan(1, "old"), plan(2, "a"), plan(3, "a"), plan(4, "b")},
	}
	out, err := run(t, opts(2), f, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"list", "random 2025-01-31", "delete 1", "random 2025-01-31",
		"random 2025-02-01", "delete 3", "random 2025-02-01", "notify",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	wantNote := notification{
		"A new meal plan has been generated",
		"A new mealplan starting on 2025-01-31 and ending 2025-02-01 has been generated",
		"https://mealie.tp-softworks.se/household/mealplan/planner/view",
	}
	if len(f.notes) != 1 || f.notes[0] != wantNote {
		t.Fatalf("notes = %v", f.notes)
	}
	for _, s := range []string{
		"Start generate '2' meals starting with '2025-01-31'",
		"Adding a new meal for '2025-02-01'",
		"Recipe 'old' has already been made the last 14 days",
		"Recipe added to meal plan: 'b'",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("log missing %q in %q", s, out)
		}
	}
}

func TestZeroCountNotifiesPriorDay(t *testing.T) {
	f := &fake{}
	if _, err := run(t, opts(0), f, context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.notes) != 1 || !strings.HasSuffix(f.notes[0].message, "starting on 2025-01-31 and ending 2025-01-30 has been generated") {
		t.Fatalf("notes = %v", f.notes)
	}
	if !reflect.DeepEqual(f.calls, []string{"list", "notify"}) {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestFailuresSkipNotification(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]*fake{
		"list":   {listErr: boom},
		"random": {randErr: boom},
		"delete": {initial: []api.MealPlan{plan(9, "x")}, randoms: []api.MealPlan{plan(1, "x")}, deleteErr: boom},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, opts(1), f, context.Background())
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if len(f.notes) != 0 {
				t.Fatal("notified after failure")
			}
		})
	}
}

func TestNotifyFailureReturned(t *testing.T) {
	boom := errors.New("boom")
	f := &fake{notifyErr: boom}
	if _, err := run(t, opts(0), f, context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancellationStopsRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fake{initial: []api.MealPlan{plan(9, "x")}}
	n := 0
	f.onRandom = func() {
		n++
		if n == 3 {
			cancel()
		}
		f.randoms = append(f.randoms, plan(1, "x"))
	}
	f.randoms = []api.MealPlan{plan(1, "x")}
	_, err := run(t, opts(1), f, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(f.notes) != 0 || n != 3 {
		t.Fatalf("notes=%v randoms=%d", f.notes, n)
	}
}
