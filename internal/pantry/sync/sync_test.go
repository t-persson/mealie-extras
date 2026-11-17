package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
	"github.com/t-persson/mealie-extras/internal/pantry/store"
)

type fakeMealie struct {
	mu       gosync.Mutex
	list     string
	events   string
	plans    string
	queries  []string
	requests []string
}

func (f *fakeMealie) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("companion must not write to Mealie: %s %s", r.Method, r.URL)
		}
		f.requests = append(f.requests, r.URL.Path)
		switch {
		case r.URL.Path == "/api/households/shopping/lists/list-1":
			fmt.Fprint(w, f.list)
		case r.URL.Path == "/api/recipes/timeline/events":
			f.queries = append(f.queries, r.URL.Query().Get("queryFilter"))
			fmt.Fprintf(w, `{"items":%s,"next":null}`, f.events)
		case r.URL.Path == "/api/households/mealplans":
			f.queries = append(f.queries, r.URL.Query().Get("start_date")+".."+r.URL.Query().Get("end_date"))
			fmt.Fprintf(w, `{"items":%s,"next":null}`, f.plans)
		case r.URL.Path == "/api/households/mealplans/11":
			fmt.Fprint(w, `{"id":11,"date":"2026-11-13","entryType":"dinner","recipeId":"r-soup","recipe":{"id":"r-soup","name":"Soup","slug":"soup"}}`)
		case r.URL.Path == "/api/recipes/soup":
			fmt.Fprint(w, `{"id":"r-soup","recipeIngredient":[
				{"quantity":2,"unit":{"id":"u-dl","name":"dl"},"food":{"id":"f-cream","name":"Cream"},"note":"","display":"2 dl cream"},
				{"quantity":null,"unit":null,"food":null,"note":"","display":"","title":"Garnish"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}
}

func newSyncer(t *testing.T, fake *fakeMealie) *Syncer {
	t.Helper()
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	return &Syncer{
		Mealie:   api.NewClientWithURLs("k", server.URL+"/api", server.URL, server.Client()),
		Store:    st,
		ListIDs:  []string{"list-1"},
		Location: stockholm,
		Now:      func() time.Time { return time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC) },
		Logger:   cli.NewLogger(io.Discard, "DEBUG", true),
	}
}

func TestSyncPurchases(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMealie{list: `{"id":"list-1","listItems":[
		{"id":"a","checked":true,"quantity":2,"food":{"id":"f-milk","name":"Milk"},"unit":{"id":"u-l","name":"l"},"note":"","display":"2 l milk"},
		{"id":"b","checked":false,"quantity":1,"food":{"id":"f-egg","name":"Egg"},"unit":null,"note":null,"display":"1 egg"},
		{"id":"c","checked":true,"quantity":0,"food":null,"unit":null,"note":" Napkins ","display":"Napkins"}]}`}
	s := newSyncer(t, fake)
	if err := s.SyncPurchases(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.Store.PendingInbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Item{}
	for _, it := range inbox {
		got[it.ItemID] = it.Item
	}
	want := map[string]store.Item{
		"a": {FoodID: "f-milk", Name: "Milk", Quantity: 2, UnitID: "u-l", UnitName: "l"},
		"c": {Name: "Napkins", Quantity: 1},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("inbox = %+v; want %+v", got, want)
	}
	if err := s.Store.AcceptPurchase(ctx, "a", store.Fridge, 0, ""); err != nil {
		t.Fatal(err)
	}
	// A repeated sync must not re-add accepted items; unchecking withdraws.
	fake.list = strings.Replace(fake.list, `"id":"c","checked":true`, `"id":"c","checked":false`, 1)
	if err := s.SyncPurchases(ctx); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := s.Store.PendingInbox(ctx); len(inbox) != 0 {
		t.Fatalf("inbox after resync = %+v", inbox)
	}
}

func TestSyncCookedMatchesMadeThisEvent(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMealie{
		// "I Made This" for 2026-11-13 local time is stored as 22:59:59 UTC.
		events: `[
			{"id":"e1","recipeId":"r-soup","subject":"Tobias made this","eventType":"comment","timestamp":"2026-11-13T22:59:59"},
			{"id":"e2","recipeId":"r-soup","subject":"Recipe updated","eventType":"info","timestamp":"2026-11-13T10:00:00Z"}]`,
		plans: `[
			{"id":10,"date":"2026-11-12","entryType":"dinner","recipeId":"r-soup","recipe":{"id":"r-soup","name":"Soup","slug":"soup"}},
			{"id":11,"date":"2026-11-13","entryType":"dinner","recipeId":"r-soup","recipe":{"id":"r-soup","name":"Soup","slug":"soup"}},
			{"id":12,"date":"2026-11-13","entryType":"side","title":"Bread","recipeId":null,"recipe":null}]`,
	}
	s := newSyncer(t, fake)
	if err := s.Store.Add(ctx, store.Fridge, store.Item{FoodID: "f-cream", Name: "Cream", Quantity: 5, UnitID: "u-dl", UnitName: "dl"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.SyncCooked(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cooked, _ := s.Store.Cooked(ctx)
	if len(cooked) != 1 || cooked[11].Source != "auto" {
		t.Fatalf("cooked = %+v", cooked)
	}
	lots, _ := s.Store.Lots(ctx, store.Fridge)
	if len(lots) != 1 || lots[0].Quantity != 3 {
		t.Fatalf("fridge = %+v", lots)
	}
	if reviews, _ := s.Store.Reviews(ctx); len(reviews) != 0 {
		t.Fatalf("reviews = %+v", reviews)
	}
	wantQueries := []string{
		`createdAt >= "2026-11-13T08:00:00"`, "2026-11-12..2026-11-14",
		`createdAt >= "2026-11-14T08:00:00"`,
	}
	if fmt.Sprint(fake.queries) != fmt.Sprint(wantQueries) {
		t.Fatalf("queries = %q; want %q", fake.queries, wantQueries)
	}
	if _, err := s.MarkCooked(ctx, 11); err == nil || !strings.Contains(err.Error(), store.ErrAlreadyCooked.Error()) {
		t.Fatalf("manual mark of auto-cooked meal = %v", err)
	}
}

func TestMarkCookedManually(t *testing.T) {
	ctx := context.Background()
	s := newSyncer(t, &fakeMealie{})
	result, err := s.MarkCooked(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consumed != 0 || result.Reviews != 1 {
		t.Fatalf("result = %+v", result)
	}
	reviews, _ := s.Store.Reviews(ctx)
	if len(reviews) != 1 || reviews[0].Name != "Cream" || reviews[0].Reason != "not in stock" {
		t.Fatalf("reviews = %+v", reviews)
	}
}
