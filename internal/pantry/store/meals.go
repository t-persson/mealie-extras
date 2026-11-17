package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// InboxItem is a purchased shopping-list item awaiting a storage decision.
type InboxItem struct {
	ItemID    string
	ListID    string
	Item      Item
	Display   string
	CreatedAt time.Time
	Status    string
}

// UpsertPurchased adds a checked shopping item to the inbox unless it is
// already known (pending, accepted or ignored). It reports whether it was added.
func (s *Store) UpsertPurchased(ctx context.Context, item InboxItem) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO inbox
		(item_id, list_id, food_id, name, quantity, unit_id, unit_name, display, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, item.ItemID, item.ListID, item.Item.FoodID, item.Item.Name,
		item.Item.Quantity, item.Item.UnitID, item.Item.UnitName, item.Display, s.stamp())
	if err != nil {
		return false, fmt.Errorf("add %s to inbox: %w", item.ItemID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ForgetPurchase is called when a shopping item is unchecked again. Pending
// entries are withdrawn; decided entries are forgotten so that checking the
// item again later counts as a new purchase.
func (s *Store) ForgetPurchase(ctx context.Context, itemID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("remove %s from inbox: %w", itemID, err)
	}
	return nil
}

// KnownPurchases returns the shopping item IDs present in the inbox table.
func (s *Store) KnownPurchases(ctx context.Context, listID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id FROM inbox WHERE list_id = ?`, listID)
	if err != nil {
		return nil, fmt.Errorf("list known purchases: %w", err)
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list known purchases: %w", err)
		}
		known[id] = true
	}
	return known, rows.Err()
}

func (s *Store) PendingInbox(ctx context.Context) ([]InboxItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, list_id, food_id, name, quantity, unit_id, unit_name,
		display, created_at, status FROM inbox WHERE status = 'pending' ORDER BY created_at, lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	defer rows.Close()
	items := []InboxItem{}
	for rows.Next() {
		var it InboxItem
		var created string
		if err := rows.Scan(&it.ItemID, &it.ListID, &it.Item.FoodID, &it.Item.Name, &it.Item.Quantity,
			&it.Item.UnitID, &it.Item.UnitName, &it.Display, &created, &it.Status); err != nil {
			return nil, fmt.Errorf("list inbox: %w", err)
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, created)
		items = append(items, it)
	}
	return items, rows.Err()
}

func pendingInboxItem(tx *sql.Tx, itemID string) (InboxItem, error) {
	var it InboxItem
	err := tx.QueryRow(`SELECT item_id, list_id, food_id, name, quantity, unit_id, unit_name, display
		FROM inbox WHERE item_id = ? AND status = 'pending'`, itemID).Scan(&it.ItemID, &it.ListID,
		&it.Item.FoodID, &it.Item.Name, &it.Item.Quantity, &it.Item.UnitID, &it.Item.UnitName, &it.Display)
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	return it, err
}

// AcceptPurchase stores a pending inbox item in location, overriding its
// quantity when quantity > 0, and remembers the location for the food.
func (s *Store) AcceptPurchase(ctx context.Context, itemID string, location Location, quantity float64, expires string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := pendingInboxItem(tx, itemID)
		if err != nil {
			return err
		}
		item := it.Item
		if quantity > 0 {
			item.Quantity = quantity
		}
		item.ExpiresOn = expires
		if item, err = normalize(item); err != nil {
			return err
		}
		opID, err := s.newOp(tx, ReasonPurchase, itemID, fmt.Sprintf("Bought %s → %s", describe(item), location))
		if err != nil {
			return err
		}
		if err := s.add(ctx, tx, opID, location, item); err != nil {
			return err
		}
		if err := s.setPref(tx, item, location); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE inbox SET status = 'accepted', op_id = ? WHERE item_id = ?`, opID, itemID)
		return err
	})
	if err != nil {
		return fmt.Errorf("store purchase %s: %w", itemID, err)
	}
	return nil
}

// IgnorePurchase dismisses a pending inbox item without changing stock.
func (s *Store) IgnorePurchase(ctx context.Context, itemID string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := pendingInboxItem(tx, itemID)
		if err != nil {
			return err
		}
		label := it.Display
		if label == "" {
			label = it.Item.Name
		}
		opID, err := s.newOp(tx, ReasonIgnore, itemID, "Ignored purchase "+label)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE inbox SET status = 'ignored', op_id = ? WHERE item_id = ?`, opID, itemID)
		return err
	})
	if err != nil {
		return fmt.Errorf("ignore purchase %s: %w", itemID, err)
	}
	return nil
}

// Need is one recipe ingredient to consume when a meal is cooked.
type Need struct {
	FoodID   string
	Name     string
	Quantity float64 // 0 when the recipe gives no amount
	UnitID   string
	UnitName string
}

// Meal describes a cooked meal-plan entry.
type Meal struct {
	PlanEntryID int
	Recipe      string
	Date        string
	Source      string // "auto" or "manual"
	EventID     string
	Needs       []Need
}

type CookResult struct {
	OpID     int64
	Consumed int
	Reviews  int
}

// ApplyCooked consumes the meal's ingredients from stock in Locations order
// (soonest expiry first within a location). Anything that cannot be consumed
// exactly is recorded for review instead of guessed.
func (s *Store) ApplyCooked(ctx context.Context, meal Meal) (CookResult, error) {
	var result CookResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM cooked WHERE plan_entry_id = ?`, meal.PlanEntryID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyCooked
		}
		opID, err := s.newOp(tx, ReasonCooked, fmt.Sprint(meal.PlanEntryID),
			fmt.Sprintf("Cooked %s (%s, %s)", meal.Recipe, meal.Date, meal.Source))
		if err != nil {
			return err
		}
		result.OpID = opID
		for _, need := range meal.Needs {
			reason, consumed, err := s.consume(ctx, tx, opID, need)
			if err != nil {
				return fmt.Errorf("consume %s: %w", need.Name, err)
			}
			if consumed {
				result.Consumed++
			}
			if reason == "" {
				continue
			}
			result.Reviews++
			if _, err := tx.Exec(`INSERT INTO review (op_id, created_at, recipe, food_id, name, quantity, unit_name, reason)
				VALUES (?,?,?,?,?,?,?,?)`, opID, s.stamp(), meal.Recipe, need.FoodID, need.Name,
				need.Quantity, need.UnitName, reason); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO cooked (plan_entry_id, at, source, event_id, op_id) VALUES (?,?,?,?,?)`,
			meal.PlanEntryID, s.stamp(), meal.Source, meal.EventID, opID)
		return err
	})
	if err != nil {
		return CookResult{}, fmt.Errorf("apply cooked meal %d: %w", meal.PlanEntryID, err)
	}
	return result, nil
}

func sameUnit(lot Lot, need Need) bool {
	if need.UnitID != "" || lot.UnitID != "" {
		return lot.UnitID == need.UnitID
	}
	return strings.EqualFold(lot.UnitName, need.UnitName)
}

func (s *Store) consume(ctx context.Context, tx *sql.Tx, opID int64, need Need) (string, bool, error) {
	if need.FoodID == "" {
		return "ingredient is not linked to a Mealie food", false, nil
	}
	lots, err := queryLots(ctx, tx, `WHERE food_id = ? ORDER BY
		CASE location WHEN 'fridge' THEN 0 WHEN 'pantry' THEN 1 ELSE 2 END,
		expires_on = '', expires_on, id`, need.FoodID)
	if err != nil {
		return "", false, err
	}
	if len(lots) == 0 {
		return "not in stock", false, nil
	}
	if !(need.Quantity > 0) {
		return "recipe gives no quantity", false, nil
	}
	remaining := need.Quantity
	matched, consumed := false, false
	var units []string
	for _, lot := range lots {
		if !sameUnit(lot, need) {
			units = append(units, unitLabel(lot.UnitName))
			continue
		}
		matched = true
		take := math.Min(remaining, lot.Quantity)
		if take > epsilon {
			if err := s.change(tx, opID, lot, -take); err != nil {
				return "", false, err
			}
			consumed = true
			remaining -= take
		}
		if remaining < epsilon {
			return "", consumed, nil
		}
	}
	if !matched {
		return fmt.Sprintf("unit mismatch: stock is in %s", strings.Join(units, ", ")), false, nil
	}
	return fmt.Sprintf("short by %s %s", FormatQuantity(remaining), need.UnitName), consumed, nil
}

func unitLabel(unit string) string {
	if unit == "" {
		return "(no unit)"
	}
	return unit
}

// CookedInfo describes how a meal-plan entry was marked cooked.
type CookedInfo struct {
	At     time.Time
	Source string
	OpID   int64
}

func (s *Store) Cooked(ctx context.Context) (map[int]CookedInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plan_entry_id, at, source, op_id FROM cooked`)
	if err != nil {
		return nil, fmt.Errorf("list cooked meals: %w", err)
	}
	defer rows.Close()
	cooked := map[int]CookedInfo{}
	for rows.Next() {
		var id int
		var info CookedInfo
		var at string
		if err := rows.Scan(&id, &at, &info.Source, &info.OpID); err != nil {
			return nil, fmt.Errorf("list cooked meals: %w", err)
		}
		info.At, _ = time.Parse(time.RFC3339, at)
		cooked[id] = info
	}
	return cooked, rows.Err()
}

// Review is an ingredient that could not be consumed automatically.
type Review struct {
	ID        int64
	CreatedAt time.Time
	Recipe    string
	FoodID    string
	Name      string
	Quantity  float64
	UnitName  string
	Reason    string
}

func (s *Store) Reviews(ctx context.Context) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, recipe, food_id, name, quantity, unit_name, reason
		FROM review WHERE resolved = 0 ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list review: %w", err)
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var r Review
		var at string
		if err := rows.Scan(&r.ID, &at, &r.Recipe, &r.FoodID, &r.Name, &r.Quantity, &r.UnitName, &r.Reason); err != nil {
			return nil, fmt.Errorf("list review: %w", err)
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, at)
		reviews = append(reviews, r)
	}
	return reviews, rows.Err()
}

// ResolveReview dismisses one review entry, or all when id is 0.
func (s *Store) ResolveReview(ctx context.Context, id int64) error {
	query, args := `UPDATE review SET resolved = 1 WHERE id = ?`, []any{id}
	if id == 0 {
		query, args = `UPDATE review SET resolved = 1`, nil
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("resolve review: %w", err)
	}
	return nil
}

// FoodLots lists stock of a Mealie food in consumption order.
func (s *Store) FoodLots(ctx context.Context, foodID string) ([]Lot, error) {
	lots, err := queryLots(ctx, s.db, `WHERE food_id = ? ORDER BY
		CASE location WHEN 'fridge' THEN 0 WHEN 'pantry' THEN 1 ELSE 2 END, expires_on = '', expires_on, id`, foodID)
	if err != nil {
		return nil, fmt.Errorf("list stock of food %s: %w", foodID, err)
	}
	return lots, nil
}
