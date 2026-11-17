// Package store persists the pantry companion's inventory in SQLite. Every
// stock change is recorded in an operation ledger so it can be undone.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Location string

const (
	Fridge  Location = "fridge"
	Pantry  Location = "pantry"
	Freezer Location = "freezer"
)

// Locations is the order stock is consumed from when a meal is cooked.
var Locations = []Location{Fridge, Pantry, Freezer}

func ParseLocation(s string) (Location, error) {
	for _, l := range Locations {
		if string(l) == s {
			return l, nil
		}
	}
	return "", fmt.Errorf("unknown location %q", s)
}

// Reasons recorded on operations.
const (
	ReasonManual   = "manual"
	ReasonPurchase = "purchase"
	ReasonIgnore   = "ignore"
	ReasonCooked   = "cooked"
	ReasonMove     = "move"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyCooked = errors.New("meal already marked cooked")
	ErrAlreadyUndone = errors.New("operation already undone")
)

const epsilon = 1e-9

// Item describes stock independent of where it is stored. FoodID is the Mealie
// food ID, or empty for free-text items.
type Item struct {
	FoodID    string
	Name      string
	Quantity  float64
	UnitID    string
	UnitName  string
	ExpiresOn string // YYYY-MM-DD or empty
}

type Lot struct {
	ID       int64
	Location Location
	Item
	CreatedAt time.Time
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// A single connection serializes writers and keeps transactions simple.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return s, nil
}

// SetClock replaces the time source; used by tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS lots (
	id INTEGER PRIMARY KEY,
	location TEXT NOT NULL CHECK (location IN ('fridge','freezer','pantry')),
	food_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	quantity REAL NOT NULL CHECK (quantity > 0),
	unit_id TEXT NOT NULL DEFAULT '',
	unit_name TEXT NOT NULL DEFAULT '',
	expires_on TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS lots_food ON lots(food_id);
CREATE TABLE IF NOT EXISTS operations (
	id INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	reason TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL,
	undone INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ledger (
	id INTEGER PRIMARY KEY,
	op_id INTEGER NOT NULL REFERENCES operations(id),
	lot_id INTEGER NOT NULL,
	location TEXT NOT NULL,
	food_id TEXT NOT NULL,
	name TEXT NOT NULL,
	unit_id TEXT NOT NULL,
	unit_name TEXT NOT NULL,
	expires_on TEXT NOT NULL,
	delta REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS ledger_op ON ledger(op_id);
CREATE TABLE IF NOT EXISTS inbox (
	item_id TEXT PRIMARY KEY,
	list_id TEXT NOT NULL,
	food_id TEXT NOT NULL,
	name TEXT NOT NULL,
	quantity REAL NOT NULL,
	unit_id TEXT NOT NULL,
	unit_name TEXT NOT NULL,
	display TEXT NOT NULL,
	created_at TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','ignored')),
	op_id INTEGER
);
CREATE TABLE IF NOT EXISTS food_prefs (
	key TEXT PRIMARY KEY,
	location TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cooked (
	plan_entry_id INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	source TEXT NOT NULL,
	event_id TEXT NOT NULL DEFAULT '',
	op_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS review (
	id INTEGER PRIMARY KEY,
	op_id INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	recipe TEXT NOT NULL,
	food_id TEXT NOT NULL,
	name TEXT NOT NULL,
	quantity REAL NOT NULL,
	unit_name TEXT NOT NULL,
	reason TEXT NOT NULL,
	resolved INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS processed (
	kind TEXT NOT NULL,
	ext_id TEXT NOT NULL,
	at TEXT NOT NULL,
	PRIMARY KEY (kind, ext_id)
);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339) }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) newOp(tx *sql.Tx, reason, ref, summary string) (int64, error) {
	res, err := tx.Exec(`INSERT INTO operations (at, reason, ref, summary) VALUES (?,?,?,?)`,
		s.stamp(), reason, ref, summary)
	if err != nil {
		return 0, fmt.Errorf("record operation: %w", err)
	}
	return res.LastInsertId()
}

const lotColumns = `id, location, food_id, name, quantity, unit_id, unit_name, expires_on, created_at`

type scanner interface{ Scan(...any) error }

func scanLot(row scanner) (Lot, error) {
	var lot Lot
	var created string
	err := row.Scan(&lot.ID, &lot.Location, &lot.FoodID, &lot.Name, &lot.Quantity,
		&lot.UnitID, &lot.UnitName, &lot.ExpiresOn, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Lot{}, ErrNotFound
	}
	if err != nil {
		return Lot{}, err
	}
	lot.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return lot, nil
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryLots(ctx context.Context, q querier, where string, args ...any) ([]Lot, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+lotColumns+` FROM lots `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lots := []Lot{}
	for rows.Next() {
		lot, err := scanLot(rows)
		if err != nil {
			return nil, err
		}
		lots = append(lots, lot)
	}
	return lots, rows.Err()
}

func lotByID(tx *sql.Tx, id int64) (Lot, error) {
	return scanLot(tx.QueryRow(`SELECT `+lotColumns+` FROM lots WHERE id = ?`, id))
}

// Lots lists stock in a location ordered by name, then soonest expiry.
func (s *Store) Lots(ctx context.Context, location Location) ([]Lot, error) {
	lots, err := queryLots(ctx, s.db,
		`WHERE location = ? ORDER BY lower(name), expires_on = '', expires_on, id`, location)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", location, err)
	}
	return lots, nil
}

// ExpiringLots lists lots that expire on or before the given date.
func (s *Store) ExpiringLots(ctx context.Context, by string) ([]Lot, error) {
	lots, err := queryLots(ctx, s.db,
		`WHERE expires_on != '' AND expires_on <= ? ORDER BY expires_on, lower(name)`, by)
	if err != nil {
		return nil, fmt.Errorf("list expiring stock: %w", err)
	}
	return lots, nil
}

func (s *Store) Lot(ctx context.Context, id int64) (Lot, error) {
	lot, err := scanLot(s.db.QueryRowContext(ctx, `SELECT `+lotColumns+` FROM lots WHERE id = ?`, id))
	if err != nil {
		return Lot{}, fmt.Errorf("get lot %d: %w", id, err)
	}
	return lot, nil
}

func (s *Store) ledger(tx *sql.Tx, opID int64, lot Lot, delta float64) error {
	_, err := tx.Exec(`INSERT INTO ledger (op_id, lot_id, location, food_id, name, unit_id, unit_name, expires_on, delta)
		VALUES (?,?,?,?,?,?,?,?,?)`, opID, lot.ID, lot.Location, lot.FoodID, lot.Name,
		lot.UnitID, lot.UnitName, lot.ExpiresOn, delta)
	if err != nil {
		return fmt.Errorf("record ledger: %w", err)
	}
	return nil
}

// change applies delta to a lot, recording it in the ledger. Lots reaching zero
// are deleted; the ledger keeps a snapshot so undo can recreate them.
func (s *Store) change(tx *sql.Tx, opID int64, lot Lot, delta float64) error {
	if math.Abs(delta) < epsilon {
		return nil
	}
	if err := s.ledger(tx, opID, lot, delta); err != nil {
		return err
	}
	return s.setQuantity(tx, lot, lot.Quantity+delta)
}

func (s *Store) setQuantity(tx *sql.Tx, lot Lot, quantity float64) error {
	if quantity < epsilon {
		_, err := tx.Exec(`DELETE FROM lots WHERE id = ?`, lot.ID)
		return err
	}
	res, err := tx.Exec(`UPDATE lots SET quantity = ? WHERE id = ?`, quantity, lot.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = tx.Exec(`INSERT INTO lots (`+lotColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			lot.ID, lot.Location, lot.FoodID, lot.Name, quantity, lot.UnitID, lot.UnitName,
			lot.ExpiresOn, s.stamp())
	}
	return err
}

func normalize(item Item) (Item, error) {
	item.Name = strings.TrimSpace(item.Name)
	item.UnitName = strings.TrimSpace(item.UnitName)
	item.ExpiresOn = strings.TrimSpace(item.ExpiresOn)
	if item.Name == "" {
		return item, errors.New("name is required")
	}
	if !(item.Quantity > 0) || math.IsInf(item.Quantity, 0) {
		return item, fmt.Errorf("quantity must be positive, got %v", item.Quantity)
	}
	if item.ExpiresOn != "" {
		if _, err := time.Parse("2006-01-02", item.ExpiresOn); err != nil {
			return item, fmt.Errorf("invalid expiry date %q", item.ExpiresOn)
		}
	}
	return item, nil
}

// add merges item into a matching lot in location or creates a new one.
func (s *Store) add(ctx context.Context, tx *sql.Tx, opID int64, location Location, item Item) error {
	lots, err := queryLots(ctx, tx, `WHERE location = ? AND food_id = ? AND unit_id = ?
		AND lower(unit_name) = lower(?) AND expires_on = ? AND (food_id != '' OR lower(name) = lower(?)) LIMIT 1`,
		location, item.FoodID, item.UnitID, item.UnitName, item.ExpiresOn, item.Name)
	if err != nil {
		return err
	}
	if len(lots) == 1 {
		return s.change(tx, opID, lots[0], item.Quantity)
	}
	res, err := tx.Exec(`INSERT INTO lots (location, food_id, name, quantity, unit_id, unit_name, expires_on, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, location, item.FoodID, item.Name, item.Quantity, item.UnitID,
		item.UnitName, item.ExpiresOn, s.stamp())
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return s.ledger(tx, opID, Lot{ID: id, Location: location, Item: item}, item.Quantity)
}

// Add manually adds stock.
func (s *Store) Add(ctx context.Context, location Location, item Item) error {
	item, err := normalize(item)
	if err != nil {
		return fmt.Errorf("add item: %w", err)
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		opID, err := s.newOp(tx, ReasonManual, "", fmt.Sprintf("Added %s to %s", describe(item), location))
		if err != nil {
			return err
		}
		if err := s.add(ctx, tx, opID, location, item); err != nil {
			return err
		}
		return s.setPref(tx, item, location)
	})
	if err != nil {
		return fmt.Errorf("add %s: %w", item.Name, err)
	}
	return nil
}

// Adjust changes a lot's quantity by delta, never below zero.
func (s *Store) Adjust(ctx context.Context, id int64, delta float64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		lot, err := lotByID(tx, id)
		if err != nil {
			return err
		}
		delta = math.Max(delta, -lot.Quantity)
		verb := "Added"
		if delta < 0 {
			verb = "Removed"
		}
		changed := lot.Item
		changed.Quantity = math.Abs(delta)
		opID, err := s.newOp(tx, ReasonManual, "", fmt.Sprintf("%s %s (%s)", verb, describe(changed), lot.Location))
		if err != nil {
			return err
		}
		return s.change(tx, opID, lot, delta)
	})
	if err != nil {
		return fmt.Errorf("adjust lot %d: %w", id, err)
	}
	return nil
}

// Remove deletes a lot entirely.
func (s *Store) Remove(ctx context.Context, id int64) error {
	lot, err := s.Lot(ctx, id)
	if err != nil {
		return err
	}
	return s.Adjust(ctx, id, -lot.Quantity)
}

// SetExpiry changes a lot's expiry date.
func (s *Store) SetExpiry(ctx context.Context, id int64, expires string) error {
	expires = strings.TrimSpace(expires)
	if expires != "" {
		if _, err := time.Parse("2006-01-02", expires); err != nil {
			return fmt.Errorf("set expiry: invalid date %q", expires)
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE lots SET expires_on = ? WHERE id = ?`, expires, id)
	if err != nil {
		return fmt.Errorf("set expiry of lot %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set expiry of lot %d: %w", id, ErrNotFound)
	}
	return nil
}

// Move transfers quantity of a lot to another location.
func (s *Store) Move(ctx context.Context, id int64, quantity float64, to Location) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		lot, err := lotByID(tx, id)
		if err != nil {
			return err
		}
		if lot.Location == to {
			return nil
		}
		if !(quantity > 0) || quantity > lot.Quantity+epsilon {
			return fmt.Errorf("quantity must be between 0 and %s", FormatQuantity(lot.Quantity))
		}
		quantity = math.Min(quantity, lot.Quantity)
		item := lot.Item
		item.Quantity = quantity
		opID, err := s.newOp(tx, ReasonMove, "", fmt.Sprintf("Moved %s from %s to %s", describe(item), lot.Location, to))
		if err != nil {
			return err
		}
		if err := s.change(tx, opID, lot, -quantity); err != nil {
			return err
		}
		if err := s.add(ctx, tx, opID, to, item); err != nil {
			return err
		}
		return s.setPref(tx, item, to)
	})
	if err != nil {
		return fmt.Errorf("move lot %d: %w", id, err)
	}
	return nil
}

func describe(item Item) string {
	unit := item.UnitName
	if unit != "" {
		unit += " "
	}
	return fmt.Sprintf("%s %s%s", FormatQuantity(item.Quantity), unit, item.Name)
}

// FormatQuantity renders quantities without trailing zeros.
func FormatQuantity(q float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", q), "0"), ".")
}

func prefKey(item Item) string {
	if item.FoodID != "" {
		return item.FoodID
	}
	return "name:" + strings.ToLower(strings.TrimSpace(item.Name))
}

func (s *Store) setPref(tx *sql.Tx, item Item, location Location) error {
	_, err := tx.Exec(`INSERT INTO food_prefs (key, location) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET location = excluded.location`, prefKey(item), location)
	return err
}

// PreferredLocation returns the last location chosen for the food, or Fridge.
func (s *Store) PreferredLocation(ctx context.Context, item Item) (Location, error) {
	var loc Location
	err := s.db.QueryRowContext(ctx, `SELECT location FROM food_prefs WHERE key = ?`, prefKey(item)).Scan(&loc)
	if errors.Is(err, sql.ErrNoRows) {
		return Fridge, nil
	}
	if err != nil {
		return "", fmt.Errorf("get preferred location: %w", err)
	}
	return loc, nil
}

// Operation is a history entry.
type Operation struct {
	ID      int64
	At      time.Time
	Reason  string
	Summary string
	Undone  bool
}

func (s *Store) Operations(ctx context.Context, limit int) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, reason, summary, undone FROM operations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	defer rows.Close()
	ops := []Operation{}
	for rows.Next() {
		var op Operation
		var at string
		if err := rows.Scan(&op.ID, &at, &op.Reason, &op.Summary, &op.Undone); err != nil {
			return nil, fmt.Errorf("list history: %w", err)
		}
		op.At, _ = time.Parse(time.RFC3339, at)
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// Undo reverses an operation's ledger entries. Undoing a purchase returns the
// item to the inbox; undoing a cooked meal allows it to be applied again.
func (s *Store) Undo(ctx context.Context, opID int64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var undone bool
		if err := tx.QueryRow(`SELECT undone FROM operations WHERE id = ?`, opID).Scan(&undone); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if undone {
			return ErrAlreadyUndone
		}
		rows, err := tx.Query(`SELECT lot_id, location, food_id, name, unit_id, unit_name, expires_on, delta
			FROM ledger WHERE op_id = ? ORDER BY id DESC`, opID)
		if err != nil {
			return err
		}
		type entry struct {
			lot   Lot
			delta float64
		}
		var entries []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.lot.ID, &e.lot.Location, &e.lot.FoodID, &e.lot.Name,
				&e.lot.UnitID, &e.lot.UnitName, &e.lot.ExpiresOn, &e.delta); err != nil {
				rows.Close()
				return err
			}
			entries = append(entries, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, e := range entries {
			current, err := lotByID(tx, e.lot.ID)
			switch {
			case errors.Is(err, ErrNotFound):
				current = e.lot
			case err != nil:
				return err
			}
			if err := s.setQuantity(tx, current, current.Quantity-e.delta); err != nil {
				return err
			}
		}
		stmts := []string{
			`UPDATE operations SET undone = 1 WHERE id = ?`,
			`UPDATE inbox SET status = 'pending', op_id = NULL WHERE op_id = ?`,
			`DELETE FROM cooked WHERE op_id = ?`,
			`DELETE FROM review WHERE op_id = ?`,
		}
		for _, stmt := range stmts {
			if _, err := tx.Exec(stmt, opID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("undo operation %d: %w", opID, err)
	}
	return nil
}

// Setting returns a stored value or "" when absent.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) IsProcessed(ctx context.Context, kind, id string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM processed WHERE kind = ? AND ext_id = ?`, kind, id).Scan(&n); err != nil {
		return false, fmt.Errorf("check processed %s %s: %w", kind, id, err)
	}
	return n > 0, nil
}

func (s *Store) MarkProcessed(ctx context.Context, kind, id string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO processed (kind, ext_id, at) VALUES (?,?,?)`, kind, id, s.stamp()); err != nil {
		return fmt.Errorf("mark processed %s %s: %w", kind, id, err)
	}
	return nil
}
