package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
	"github.com/t-persson/mealie-extras/internal/pantry/store"
)

type fakeCatalog struct{ entries []api.PlanEntry }

func (fakeCatalog) Foods(context.Context) ([]api.Ref, error) {
	return []api.Ref{{ID: "food-milk", Name: "Milk"}}, nil
}
func (fakeCatalog) Units(context.Context) ([]api.Ref, error) {
	return []api.Ref{{ID: "unit-l", Name: "liter"}}, nil
}
func (c fakeCatalog) PlanEntries(context.Context, string, string) ([]api.PlanEntry, error) {
	return c.entries, nil
}

type fakeSyncer struct {
	triggered int
	cooked    []int
}

func (f *fakeSyncer) Trigger()                      { f.triggered++ }
func (f *fakeSyncer) SyncAll(context.Context) error { return nil }
func (f *fakeSyncer) MarkCooked(_ context.Context, id int) (store.CookResult, error) {
	f.cooked = append(f.cooked, id)
	return store.CookResult{Consumed: 2, Reviews: 1}, nil
}

func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *fakeSyncer) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pantry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	syncer := &fakeSyncer{}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		Store: st, Syncer: syncer, HookSecret: "s3cret", Location: time.UTC,
		Now:    func() time.Time { return now },
		Logger: cli.NewLogger(io.Discard, "INFO", true),
		Catalog: fakeCatalog{entries: []api.PlanEntry{
			{ID: 7, Date: "2026-03-09", EntryType: "dinner", Recipe: &api.PlanRecipe{ID: "r1", Slug: "pancakes", Name: "Pancakes"}},
			{ID: 8, Date: "2026-03-11", EntryType: "lunch", Title: "Leftovers"},
		}},
	}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, st, syncer
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func post(t *testing.T, ts *httptest.Server, path string, form url.Values, header http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func get(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return string(body)
}

func TestHookRequiresSecret(t *testing.T) {
	ts, _, syncer := newTestServer(t)
	if resp := post(t, ts, "/hooks/mealie/wrong", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong secret: %d", resp.StatusCode)
	}
	if resp := post(t, ts, "/hooks/mealie/s3cret", nil, http.Header{"Origin": {"https://elsewhere"}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid secret: %d", resp.StatusCode)
	}
	if syncer.triggered != 1 {
		t.Fatalf("triggered %d times", syncer.triggered)
	}
}

func TestCrossSitePostRefused(t *testing.T) {
	ts, st, _ := newTestServer(t)
	form := url.Values{"name": {"Milk"}, "location": {"fridge"}}
	for _, h := range []http.Header{{"Sec-Fetch-Site": {"cross-site"}}, {"Origin": {"https://evil.example"}}} {
		if resp := post(t, ts, "/add", form, h); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%v: status %d", h, resp.StatusCode)
		}
	}
	lots, _ := st.Lots(context.Background(), store.Fridge)
	if len(lots) != 0 {
		t.Fatalf("cross-site add stored %v", lots)
	}
}

func TestAddLinksFoodAndShowsOnOverview(t *testing.T) {
	ts, st, _ := newTestServer(t)
	form := url.Values{"name": {"milk"}, "quantity": {"1,5"}, "unit": {"Liter"}, "location": {"fridge"}, "expires": {"2026-03-12"}, "back": {"/add?location=fridge"}}
	resp := post(t, ts, "/add", form, http.Header{"Sec-Fetch-Site": {"same-origin"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/add?location=fridge&msg=") {
		t.Fatalf("redirect %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	lots, _ := st.Lots(context.Background(), store.Fridge)
	if len(lots) != 1 || lots[0].FoodID != "food-milk" || lots[0].UnitID != "unit-l" || lots[0].Quantity != 1.5 {
		t.Fatalf("lots %+v", lots)
	}
	body := get(t, ts, "/")
	for _, want := range []string{"Milk", "1.5 liter", "Expiring by 2026-03-13", `class="lot soon"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestBackRejectsOffsiteRedirect(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp := post(t, ts, "/sync", url.Values{"back": {"//evil.example/x"}}, nil)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/?msg=") {
		t.Fatalf("redirected to %q", loc)
	}
}

func TestMealsAndMarkCooked(t *testing.T) {
	ts, _, syncer := newTestServer(t)
	body := get(t, ts, "/meals")
	if !strings.Contains(body, "Pancakes") || !strings.Contains(body, "Leftovers") || !strings.Contains(body, `action="/meals/7/cooked"`) {
		t.Fatalf("meals page: %s", body)
	}
	if strings.Contains(body, `action="/meals/8/cooked"`) {
		t.Fatal("non-recipe entry offered as cookable")
	}
	resp := post(t, ts, "/meals/7/cooked", nil, nil)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "2+ingredient") || !strings.Contains(loc, "1+need+review") {
		t.Fatalf("redirect %q", loc)
	}
	if len(syncer.cooked) != 1 || syncer.cooked[0] != 7 {
		t.Fatalf("cooked %v", syncer.cooked)
	}
}

func TestPagesRender(t *testing.T) {
	ts, st, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := st.UpsertPurchased(ctx, store.InboxItem{ItemID: "i1", ListID: "l1", Item: store.Item{Name: "Butter", Quantity: 2}, Display: "2 Butter"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(ctx, store.Freezer, store.Item{Name: "Peas", Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/inbox": "Butter", "/history": "Peas", "/review": "Nothing to review", "/add": `value="Milk"`,
		"/static/style.css": "--accent", "/healthz": "ok",
	} {
		if body := get(t, ts, path); !strings.Contains(body, want) {
			t.Errorf("%s missing %q", path, want)
		}
	}
	resp := post(t, ts, "/inbox/i1/accept", url.Values{"location": {"pantry"}, "quantity": {""}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=Stored") {
		t.Fatalf("accept redirect %q", resp.Header.Get("Location"))
	}
	lots, _ := st.Lots(ctx, store.Pantry)
	if len(lots) != 1 || lots[0].Name != "Butter" || lots[0].Quantity != 2 {
		t.Fatalf("pantry %+v", lots)
	}
}
