// Package sync keeps the pantry in step with Mealie: checked shopping-list
// items become inbox entries and cooked meal-plan entries consume stock.
package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	gosync "sync"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
	"github.com/t-persson/mealie-extras/internal/pantry/store"
)

// Mealie is the subset of the Mealie API the companion reads. It never writes.
type Mealie interface {
	ShoppingListItems(ctx context.Context, listID string) ([]api.ShoppingItem, error)
	TimelineEvents(ctx context.Context, since time.Time) ([]api.TimelineEvent, error)
	PlanEntries(ctx context.Context, startDate, endDate string) ([]api.PlanEntry, error)
	PlanEntry(ctx context.Context, id int) (api.PlanEntry, error)
	RecipeIngredients(ctx context.Context, slug string) ([]api.Ingredient, error)
}

const (
	dateLayout = "2006-01-02"
	cursorKey  = "timeline_cursor"
	// madeThisType is the eventType Mealie's "I Made This" dialog creates.
	madeThisType = "comment"
	// cursorOverlap re-reads recent events to tolerate clock skew; processed
	// events are skipped.
	cursorOverlap = time.Hour
	// firstLookback bounds how much history is applied on the first run.
	firstLookback = 24 * time.Hour
)

type Syncer struct {
	Mealie   Mealie
	Store    *store.Store
	ListIDs  []string
	Location *time.Location // local time zone used for meal-plan dates
	Now      func() time.Time
	Logger   *cli.Logger

	mu      gosync.Mutex
	trigger chan struct{}
	once    gosync.Once
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Syncer) triggers() chan struct{} {
	s.once.Do(func() { s.trigger = make(chan struct{}, 1) })
	return s.trigger
}

// Trigger requests a sync soon without blocking; bursts collapse into one run.
func (s *Syncer) Trigger() {
	select {
	case s.triggers() <- struct{}{}:
	default:
	}
}

// Run syncs immediately, then on every interval or Trigger until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.SyncAll(ctx); err != nil && ctx.Err() == nil {
			s.Logger.Info("sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.triggers():
		}
	}
}

// SyncAll runs both purchase and cooked syncs, returning all failures.
func (s *Syncer) SyncAll(ctx context.Context) error {
	return errors.Join(s.SyncPurchases(ctx), s.SyncCooked(ctx))
}

// SyncPurchases moves newly checked shopping-list items into the inbox.
// Unchecking an item withdraws it so a later check counts as a new purchase.
func (s *Syncer) SyncPurchases(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, listID := range s.ListIDs {
		added, err := s.syncList(ctx, listID)
		if err != nil {
			errs = append(errs, fmt.Errorf("sync purchases from list %s: %w", listID, err))
			continue
		}
		if added > 0 {
			s.Logger.Info("Added %d purchased item(s) from list %s to the inbox", added, listID)
		}
	}
	return errors.Join(errs...)
}

func (s *Syncer) syncList(ctx context.Context, listID string) (int, error) {
	items, err := s.Mealie.ShoppingListItems(ctx, listID)
	if err != nil {
		return 0, err
	}
	known, err := s.Store.KnownPurchases(ctx, listID)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, item := range items {
		if !item.Checked {
			if known[item.ID] {
				if err := s.Store.ForgetPurchase(ctx, item.ID); err != nil {
					return added, err
				}
			}
			continue
		}
		if known[item.ID] {
			continue
		}
		ok, err := s.Store.UpsertPurchased(ctx, purchase(listID, item))
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

func purchase(listID string, item api.ShoppingItem) store.InboxItem {
	stock := store.Item{Quantity: item.Quantity}
	if stock.Quantity <= 0 {
		stock.Quantity = 1
	}
	if item.Food != nil {
		stock.FoodID, stock.Name = item.Food.ID, item.Food.Name
	}
	if item.Unit != nil {
		stock.UnitID, stock.UnitName = item.Unit.ID, item.Unit.Name
	}
	for _, name := range []string{stock.Name, item.Note, item.Display} {
		if strings.TrimSpace(name) != "" {
			stock.Name = strings.TrimSpace(name)
			break
		}
	}
	if stock.Name == "" {
		stock.Name = "Unnamed item"
	}
	return store.InboxItem{ItemID: item.ID, ListID: listID, Item: stock, Display: item.Display}
}

// SyncCooked applies new "I Made This" events to the matching meal-plan entry.
func (s *Syncer) SyncCooked(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := s.now()
	cursor, err := s.Store.Setting(ctx, cursorKey)
	if err != nil {
		return fmt.Errorf("sync cooked meals: %w", err)
	}
	since := started.Add(-firstLookback)
	if cursor != "" {
		if since, err = time.Parse(time.RFC3339, cursor); err != nil {
			return fmt.Errorf("sync cooked meals: invalid cursor %q: %w", cursor, err)
		}
	}
	events, err := s.Mealie.TimelineEvents(ctx, since.Add(-cursorOverlap))
	if err != nil {
		return fmt.Errorf("sync cooked meals: %w", err)
	}
	for _, event := range events {
		if event.EventType != madeThisType || event.RecipeID == "" {
			continue
		}
		done, err := s.Store.IsProcessed(ctx, "timeline", event.ID)
		if err != nil {
			return fmt.Errorf("sync cooked meals: %w", err)
		}
		if done {
			continue
		}
		if err := s.applyEvent(ctx, event); err != nil {
			return fmt.Errorf("sync cooked meals: event %s: %w", event.ID, err)
		}
		if err := s.Store.MarkProcessed(ctx, "timeline", event.ID); err != nil {
			return fmt.Errorf("sync cooked meals: %w", err)
		}
	}
	if err := s.Store.SetSetting(ctx, cursorKey, started.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("sync cooked meals: %w", err)
	}
	return nil
}

func (s *Syncer) applyEvent(ctx context.Context, event api.TimelineEvent) error {
	at, err := parseTimestamp(event.Timestamp)
	if err != nil {
		return err
	}
	day := at.In(s.loc())
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.loc())
	entries, err := s.Mealie.PlanEntries(ctx, day.AddDate(0, 0, -1).Format(dateLayout), day.AddDate(0, 0, 1).Format(dateLayout))
	if err != nil {
		return err
	}
	cooked, err := s.Store.Cooked(ctx)
	if err != nil {
		return err
	}
	// Prefer the event's own date, then the day before, then the day after.
	for _, offset := range []int{0, -1, 1} {
		date := day.AddDate(0, 0, offset).Format(dateLayout)
		for _, entry := range entries {
			if entry.Date != date || recipeID(entry) != event.RecipeID {
				continue
			}
			if _, done := cooked[entry.ID]; done {
				continue
			}
			result, err := s.cook(ctx, entry, "auto", event.ID)
			if errors.Is(err, store.ErrAlreadyCooked) {
				continue
			}
			if err != nil {
				return err
			}
			s.Logger.Info("Marked %s on %s cooked from Mealie timeline: %d consumed, %d for review",
				entry.Recipe.Name, entry.Date, result.Consumed, result.Reviews)
			return nil
		}
	}
	s.Logger.Debug("Timeline event %s for recipe %s has no uncooked meal-plan entry near %s",
		event.ID, event.RecipeID, day.Format(dateLayout))
	return nil
}

func recipeID(entry api.PlanEntry) string {
	if entry.RecipeID != "" {
		return entry.RecipeID
	}
	if entry.Recipe != nil {
		return entry.Recipe.ID
	}
	return ""
}

// parseTimestamp accepts RFC 3339 or Mealie's naive UTC timestamps.
func parseTimestamp(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return t, nil
}

// MarkCooked manually marks a meal-plan entry cooked.
func (s *Syncer) MarkCooked(ctx context.Context, planEntryID int) (store.CookResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.Mealie.PlanEntry(ctx, planEntryID)
	if err != nil {
		return store.CookResult{}, fmt.Errorf("mark meal %d cooked: %w", planEntryID, err)
	}
	result, err := s.cook(ctx, entry, "manual", "")
	if err != nil {
		return store.CookResult{}, fmt.Errorf("mark meal %d cooked: %w", planEntryID, err)
	}
	return result, nil
}

func (s *Syncer) cook(ctx context.Context, entry api.PlanEntry, source, eventID string) (store.CookResult, error) {
	if entry.Recipe == nil || entry.Recipe.Slug == "" {
		return store.CookResult{}, errors.New("meal-plan entry has no recipe")
	}
	ingredients, err := s.Mealie.RecipeIngredients(ctx, entry.Recipe.Slug)
	if err != nil {
		return store.CookResult{}, err
	}
	meal := store.Meal{PlanEntryID: entry.ID, Recipe: entry.Recipe.Name, Date: entry.Date,
		Source: source, EventID: eventID}
	for _, ing := range ingredients {
		if n := need(ing); n.FoodID != "" || n.Name != "" {
			meal.Needs = append(meal.Needs, n)
		}
	}
	return s.Store.ApplyCooked(ctx, meal)
}

func need(ing api.Ingredient) store.Need {
	var n store.Need
	if ing.Quantity != nil {
		n.Quantity = *ing.Quantity
	}
	if ing.Unit != nil {
		n.UnitID, n.UnitName = ing.Unit.ID, ing.Unit.Name
	}
	if ing.Food != nil {
		n.FoodID, n.Name = ing.Food.ID, ing.Food.Name
	}
	for _, name := range []string{n.Name, ing.Note, ing.Display} {
		if strings.TrimSpace(name) != "" {
			n.Name = strings.TrimSpace(name)
			break
		}
	}
	return n
}
