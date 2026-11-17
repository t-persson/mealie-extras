// Package web serves the pantry companion's HTML interface and Mealie hook.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
	"github.com/t-persson/mealie-extras/internal/pantry/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Catalog provides Mealie foods, units and meal-plan entries for the UI.
type Catalog interface {
	Foods(ctx context.Context) ([]api.Ref, error)
	Units(ctx context.Context) ([]api.Ref, error)
	PlanEntries(ctx context.Context, startDate, endDate string) ([]api.PlanEntry, error)
}

// Syncer is the subset of the sync service used by handlers.
type Syncer interface {
	Trigger()
	SyncAll(ctx context.Context) error
	MarkCooked(ctx context.Context, planEntryID int) (store.CookResult, error)
}

type Server struct {
	Store      *store.Store
	Catalog    Catalog
	Syncer     Syncer
	HookSecret string
	MealieURL  string // public Mealie base URL for links
	Location   *time.Location
	Now        func() time.Time
	Logger     *cli.Logger

	tmpl     *template.Template
	cacheMu  gosync.Mutex
	foods    []api.Ref
	units    []api.Ref
	cachedAt time.Time
}

const catalogTTL = 10 * time.Minute

var funcs = template.FuncMap{
	"qty":   store.FormatQuantity,
	"title": func(l store.Location) string { return strings.ToUpper(string(l[:1])) + string(l[1:]) },
	"locations": func() []store.Location {
		return store.Locations
	},
	"lotView": func(lot store.Lot, d overviewData) lotView {
		return lotView{Lot: lot, Today: d.Today, Soon: d.Soon}
	},
}

type lotView struct {
	Lot         store.Lot
	Today, Soon string
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /hooks/mealie/{secret}", s.hook)
	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /add", s.addForm)
	mux.HandleFunc("POST /add", s.add)
	mux.HandleFunc("POST /lots/{id}/adjust", s.adjust)
	mux.HandleFunc("POST /lots/{id}/move", s.move)
	mux.HandleFunc("POST /lots/{id}/expiry", s.expiry)
	mux.HandleFunc("POST /lots/{id}/remove", s.remove)
	mux.HandleFunc("GET /inbox", s.inbox)
	mux.HandleFunc("POST /inbox/{id}/accept", s.accept)
	mux.HandleFunc("POST /inbox/{id}/ignore", s.ignore)
	mux.HandleFunc("POST /inbox/accept-all", s.acceptAll)
	mux.HandleFunc("GET /meals", s.meals)
	mux.HandleFunc("POST /meals/{id}/cooked", s.cooked)
	mux.HandleFunc("GET /review", s.review)
	mux.HandleFunc("POST /review/{id}/resolve", s.resolve)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("POST /history/{id}/undo", s.undo)
	mux.HandleFunc("POST /sync", s.syncNow)
	return sameOrigin(mux), nil
}

// sameOrigin rejects cross-site form posts. The hook is authenticated by its secret.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.URL.Path, "/hooks/") {
			site := r.Header.Get("Sec-Fetch-Site")
			origin := r.Header.Get("Origin")
			crossSite := site != "" && site != "same-origin" && site != "none"
			if !crossSite && site == "" && origin != "" {
				u, err := url.Parse(origin)
				crossSite = err != nil || !strings.EqualFold(u.Host, r.Host)
			}
			if crossSite {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) today() time.Time {
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}
	n := s.now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

type page struct {
	Title   string
	Nav     string
	Message string
	Error   string
	Inbox   int
	Review  int
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	p := page{Title: title, Nav: name, Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"), Data: data}
	if inbox, err := s.Store.PendingInbox(r.Context()); err == nil {
		p.Inbox = len(inbox)
	}
	if reviews, err := s.Store.Reviews(r.Context()); err == nil {
		p.Review = len(reviews)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name+".html", p); err != nil {
		s.Logger.Info("render %s: %v", name, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Logger.Info("%s %s: %v", r.Method, r.URL.Path, err)
	status := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}

// back redirects to the form's "back" path with a message or error.
func (s *Server) back(w http.ResponseWriter, r *http.Request, fallback, msg string, err error) {
	target := r.PostFormValue("back")
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") {
		target = fallback
	}
	u, _ := url.Parse(target)
	q := u.Query()
	q.Del("msg")
	q.Del("err")
	if err != nil {
		s.Logger.Info("%s %s: %v", r.Method, r.URL.Path, err)
		q.Set("err", err.Error())
	} else if msg != "" {
		q.Set("msg", msg)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", r.PathValue("id"))
	}
	return id, nil
}

func parseQuantity(value string) (float64, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, ",", "."))
	if value == "" {
		return 0, nil
	}
	q, err := strconv.ParseFloat(value, 64)
	if err != nil || q < 0 {
		return 0, fmt.Errorf("invalid quantity %q", value)
	}
	return q, nil
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	if s.HookSecret == "" || subtle.ConstantTimeCompare([]byte(r.PathValue("secret")), []byte(s.HookSecret)) != 1 {
		http.NotFound(w, r)
		return
	}
	// The notification body is not trusted; it only signals that a list changed.
	s.Syncer.Trigger()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	err := s.Syncer.SyncAll(r.Context())
	s.back(w, r, "/", "Synced with Mealie", err)
}

type overviewData struct {
	Sections []section
	Expiring []store.Lot
	Today    string
	Soon     string
}

type section struct {
	Location store.Location
	Lots     []store.Lot
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	data := overviewData{Today: today.Format("2006-01-02"), Soon: today.AddDate(0, 0, 3).Format("2006-01-02")}
	for _, loc := range store.Locations {
		lots, err := s.Store.Lots(r.Context(), loc)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Sections = append(data.Sections, section{Location: loc, Lots: lots})
	}
	expiring, err := s.Store.ExpiringLots(r.Context(), data.Soon)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data.Expiring = expiring
	s.render(w, r, "overview", "Stock", data)
}

// catalog returns cached Mealie foods and units; failures yield empty lists so
// free-text entry keeps working while Mealie is unreachable.
func (s *Server) catalog(ctx context.Context) ([]api.Ref, []api.Ref) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cachedAt.IsZero() || s.now().Sub(s.cachedAt) > catalogTTL {
		foods, ferr := s.Catalog.Foods(ctx)
		units, uerr := s.Catalog.Units(ctx)
		if err := errors.Join(ferr, uerr); err != nil {
			s.Logger.Info("load Mealie foods and units: %v", err)
			return s.foods, s.units
		}
		s.foods, s.units, s.cachedAt = foods, units, s.now()
	}
	return s.foods, s.units
}

func findRef(refs []api.Ref, name string) (api.Ref, bool) {
	for _, ref := range refs {
		if strings.EqualFold(ref.Name, name) {
			return ref, true
		}
	}
	return api.Ref{}, false
}

type addData struct {
	Foods    []api.Ref
	Units    []api.Ref
	Location store.Location
}

func (s *Server) addForm(w http.ResponseWriter, r *http.Request) {
	foods, units := s.catalog(r.Context())
	loc, err := store.ParseLocation(r.URL.Query().Get("location"))
	if err != nil {
		loc = store.Fridge
	}
	s.render(w, r, "add", "Add item", addData{Foods: foods, Units: units, Location: loc})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	loc, err := store.ParseLocation(r.PostFormValue("location"))
	if err != nil {
		s.back(w, r, "/add", "", err)
		return
	}
	quantity, err := parseQuantity(r.PostFormValue("quantity"))
	if err != nil {
		s.back(w, r, "/add", "", err)
		return
	}
	if quantity == 0 {
		quantity = 1
	}
	foods, units := s.catalog(r.Context())
	item := store.Item{
		Name: strings.TrimSpace(r.PostFormValue("name")), Quantity: quantity,
		UnitName: strings.TrimSpace(r.PostFormValue("unit")), ExpiresOn: r.PostFormValue("expires"),
	}
	if food, ok := findRef(foods, item.Name); ok {
		item.FoodID, item.Name = food.ID, food.Name
	}
	if unit, ok := findRef(units, item.UnitName); ok && item.UnitName != "" {
		item.UnitID, item.UnitName = unit.ID, unit.Name
	}
	if err := s.Store.Add(r.Context(), loc, item); err != nil {
		s.back(w, r, "/add", "", err)
		return
	}
	s.back(w, r, "/#"+string(loc), fmt.Sprintf("Added %s to %s", item.Name, loc), nil)
}

func (s *Server) adjust(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		var delta float64
		if remove := r.PostFormValue("remove"); remove != "" {
			delta, err = parseQuantity(remove)
			delta = -delta
		} else {
			delta, err = strconv.ParseFloat(r.PostFormValue("delta"), 64)
		}
		if err == nil {
			err = s.Store.Adjust(r.Context(), id, delta)
		}
	}
	s.back(w, r, "/", "", err)
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	var to store.Location
	var quantity float64
	if err == nil {
		to, err = store.ParseLocation(r.PostFormValue("to"))
	}
	if err == nil {
		quantity, err = parseQuantity(r.PostFormValue("quantity"))
	}
	if err == nil {
		if quantity == 0 {
			var lot store.Lot
			if lot, err = s.Store.Lot(r.Context(), id); err == nil {
				quantity = lot.Quantity
			}
		}
		if err == nil {
			err = s.Store.Move(r.Context(), id, quantity, to)
		}
	}
	s.back(w, r, "/", "Moved to "+string(to), err)
}

func (s *Server) expiry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.SetExpiry(r.Context(), id, r.PostFormValue("expires"))
	}
	s.back(w, r, "/", "Expiry updated", err)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.Remove(r.Context(), id)
	}
	s.back(w, r, "/", "Removed", err)
}

type inboxRow struct {
	store.InboxItem
	Suggested store.Location
}

func (s *Server) inboxRows(ctx context.Context) ([]inboxRow, error) {
	items, err := s.Store.PendingInbox(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]inboxRow, 0, len(items))
	for _, it := range items {
		loc, err := s.Store.PreferredLocation(ctx, it.Item)
		if err != nil {
			return nil, err
		}
		rows = append(rows, inboxRow{InboxItem: it, Suggested: loc})
	}
	return rows, nil
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inboxRows(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "inbox", "Inbox", rows)
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	loc, err := store.ParseLocation(r.PostFormValue("location"))
	var quantity float64
	if err == nil {
		quantity, err = parseQuantity(r.PostFormValue("quantity"))
	}
	if err == nil {
		err = s.Store.AcceptPurchase(r.Context(), r.PathValue("id"), loc, quantity, r.PostFormValue("expires"))
	}
	s.back(w, r, "/inbox", "Stored in "+string(loc), err)
}

func (s *Server) ignore(w http.ResponseWriter, r *http.Request) {
	err := s.Store.IgnorePurchase(r.Context(), r.PathValue("id"))
	s.back(w, r, "/inbox", "Ignored", err)
}

func (s *Server) acceptAll(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inboxRows(r.Context())
	n := 0
	for _, row := range rows {
		if err != nil {
			break
		}
		err = s.Store.AcceptPurchase(r.Context(), row.ItemID, row.Suggested, 0, "")
		if err == nil {
			n++
		}
	}
	s.back(w, r, "/inbox", fmt.Sprintf("Stored %d item(s)", n), err)
}

type mealRow struct {
	Entry  api.PlanEntry
	Name   string
	Cooked *store.CookedInfo
	Past   bool
}

type mealsData struct {
	Days      []mealDay
	MealieURL string
}

type mealDay struct {
	Date  string
	Meals []mealRow
}

func (s *Server) meals(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	start, end := today.AddDate(0, 0, -7), today.AddDate(0, 0, 7)
	entries, err := s.Catalog.PlanEntries(r.Context(), start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		s.Logger.Info("%s %s: %v", r.Method, r.URL.Path, err)
		q := r.URL.Query()
		q.Set("err", "Could not load the meal plan from Mealie: "+err.Error())
		r.URL.RawQuery = q.Encode()
		s.render(w, r, "meals", "Meal plan", mealsData{MealieURL: s.MealieURL})
		return
	}
	cooked, err := s.Store.Cooked(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Date > entries[j].Date })
	data := mealsData{MealieURL: s.MealieURL}
	for _, e := range entries {
		row := mealRow{Entry: e, Name: e.Title, Past: e.Date <= today.Format("2006-01-02")}
		if e.Recipe != nil {
			row.Name = e.Recipe.Name
		}
		if info, ok := cooked[e.ID]; ok {
			row.Cooked = &info
		}
		if n := len(data.Days); n == 0 || data.Days[n-1].Date != e.Date {
			data.Days = append(data.Days, mealDay{Date: e.Date})
		}
		day := &data.Days[len(data.Days)-1]
		day.Meals = append(day.Meals, row)
	}
	s.render(w, r, "meals", "Meal plan", data)
}

func (s *Server) cooked(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		s.back(w, r, "/meals", "", fmt.Errorf("invalid id %q", r.PathValue("id")))
		return
	}
	result, err := s.Syncer.MarkCooked(r.Context(), id)
	msg := fmt.Sprintf("Marked cooked: %d ingredient(s) taken from stock", result.Consumed)
	if result.Reviews > 0 {
		msg += fmt.Sprintf(", %d need review", result.Reviews)
	}
	s.back(w, r, "/meals", msg, err)
}

type reviewRow struct {
	store.Review
	Lots []store.Lot
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	reviews, err := s.Store.Reviews(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows := make([]reviewRow, 0, len(reviews))
	for _, rv := range reviews {
		row := reviewRow{Review: rv}
		if rv.FoodID != "" {
			if row.Lots, err = s.Store.FoodLots(r.Context(), rv.FoodID); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		rows = append(rows, row)
	}
	s.render(w, r, "review", "Review", rows)
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var id int64
	var err error
	if r.PathValue("id") != "all" {
		id, err = pathID(r)
	}
	if err == nil {
		err = s.Store.ResolveReview(r.Context(), id)
	}
	s.back(w, r, "/review", "Dismissed", err)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	ops, err := s.Store.Operations(r.Context(), 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "history", "History", ops)
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.Undo(r.Context(), id)
	}
	s.back(w, r, "/history", "Undone", err)
}
