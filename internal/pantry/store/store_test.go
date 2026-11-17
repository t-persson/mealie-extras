package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pantry.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) })
	t.Cleanup(func() { s.Close() })
	return s
}

func quantities(t *testing.T, s *Store, loc Location) map[string]float64 {
	t.Helper()
	lots, err := s.Lots(context.Background(), loc)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, lot := range lots {
		got[lot.Name+"|"+lot.UnitName+"|"+lot.ExpiresOn] += lot.Quantity
	}
	return got
}

func mustLot(t *testing.T, s *Store, loc Location) Lot {
	t.Helper()
	lots, err := s.Lots(context.Background(), loc)
	if err != nil || len(lots) != 1 {
		t.Fatalf("lots in %s = %+v, %v", loc, lots, err)
	}
	return lots[0]
}

func TestAddMergesAndAdjustClamps(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	milk := Item{FoodID: "f-milk", Name: "Milk", Quantity: 1, UnitID: "u-l", UnitName: "l"}
	for i := 0; i < 2; i++ {
		if err := s.Add(ctx, Fridge, milk); err != nil {
			t.Fatal(err)
		}
	}
	other := milk
	other.ExpiresOn = "2026-11-20"
	if err := s.Add(ctx, Fridge, other); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"Milk|l|": 2, "Milk|l|2026-11-20": 1}
	if got := quantities(t, s, Fridge); !equal(got, want) {
		t.Fatalf("fridge = %v; want %v", got, want)
	}
	lots, _ := s.Lots(ctx, Fridge)
	if err := s.Adjust(ctx, lots[0].ID, -5); err != nil {
		t.Fatal(err)
	}
	if got := quantities(t, s, Fridge); len(got) != 1 {
		t.Fatalf("fridge after removing = %v", got)
	}
	if err := s.Add(ctx, Fridge, Item{Name: " ", Quantity: 1}); err == nil {
		t.Fatal("expected error for empty name")
	}
	if err := s.Add(ctx, Fridge, Item{Name: "x", Quantity: 0}); err == nil {
		t.Fatal("expected error for zero quantity")
	}
	if err := s.Add(ctx, Fridge, Item{Name: "x", Quantity: 1, ExpiresOn: "tomorrow"}); err == nil {
		t.Fatal("expected error for invalid expiry")
	}
}

func TestMoveAndUndo(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.Add(ctx, Fridge, Item{Name: "Bread", Quantity: 3}); err != nil {
		t.Fatal(err)
	}
	lot := mustLot(t, s, Fridge)
	if err := s.Move(ctx, lot.ID, 3, Freezer); err != nil {
		t.Fatal(err)
	}
	if got := quantities(t, s, Fridge); len(got) != 0 {
		t.Fatalf("fridge = %v", got)
	}
	if loc, _ := s.PreferredLocation(ctx, Item{Name: "bread"}); loc != Freezer {
		t.Fatalf("preferred = %s", loc)
	}
	ops, _ := s.Operations(ctx, 10)
	if err := s.Undo(ctx, ops[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := mustLot(t, s, Fridge); got.ID != lot.ID || got.Quantity != 3 {
		t.Fatalf("restored lot = %+v", got)
	}
	if got := quantities(t, s, Freezer); len(got) != 0 {
		t.Fatalf("freezer = %v", got)
	}
	if err := s.Undo(ctx, ops[0].ID); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("second undo = %v", err)
	}
	if err := s.Move(ctx, lot.ID, 4, Freezer); err == nil {
		t.Fatal("expected error moving more than available")
	}
}

func TestPurchaseInboxFlow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	item := InboxItem{ItemID: "i1", ListID: "l", Item: Item{FoodID: "f-peas", Name: "Peas", Quantity: 2, UnitName: "bag"}}
	if added, err := s.UpsertPurchased(ctx, item); err != nil || !added {
		t.Fatalf("added = %v, %v", added, err)
	}
	if added, _ := s.UpsertPurchased(ctx, item); added {
		t.Fatal("duplicate inbox entry")
	}
	if err := s.AcceptPurchase(ctx, "i1", Freezer, 0, "2027-01-01"); err != nil {
		t.Fatal(err)
	}
	if got := quantities(t, s, Freezer); got["Peas|bag|2027-01-01"] != 2 {
		t.Fatalf("freezer = %v", got)
	}
	if pending, _ := s.PendingInbox(ctx); len(pending) != 0 {
		t.Fatalf("pending = %v", pending)
	}
	if err := s.AcceptPurchase(ctx, "i1", Freezer, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accept twice = %v", err)
	}
	if loc, _ := s.PreferredLocation(ctx, Item{FoodID: "f-peas"}); loc != Freezer {
		t.Fatalf("preferred = %s", loc)
	}
	ops, _ := s.Operations(ctx, 1)
	if err := s.Undo(ctx, ops[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingInbox(ctx); len(pending) != 1 {
		t.Fatalf("pending after undo = %v", pending)
	}
	if err := s.IgnorePurchase(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if got := quantities(t, s, Freezer); len(got) != 0 {
		t.Fatalf("freezer = %v", got)
	}
	if err := s.ForgetPurchase(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if added, _ := s.UpsertPurchased(ctx, item); !added {
		t.Fatal("forgotten purchase should be re-added")
	}
}

func TestApplyCooked(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	add := func(loc Location, item Item) {
		t.Helper()
		if err := s.Add(ctx, loc, item); err != nil {
			t.Fatal(err)
		}
	}
	add(Freezer, Item{FoodID: "mince", Name: "Mince", Quantity: 500, UnitID: "g", UnitName: "g"})
	add(Fridge, Item{FoodID: "mince", Name: "Mince", Quantity: 300, UnitID: "g", UnitName: "g"})
	add(Pantry, Item{FoodID: "pasta", Name: "Pasta", Quantity: 1, UnitID: "pkt", UnitName: "packet"})
	add(Fridge, Item{FoodID: "cream", Name: "Cream", Quantity: 1, UnitID: "dl", UnitName: "dl"})
	add(Fridge, Item{FoodID: "egg", Name: "Egg", Quantity: 2})
	meal := Meal{PlanEntryID: 7, Recipe: "Bolognese", Date: "2026-11-14", Source: "manual", Needs: []Need{
		{FoodID: "mince", Name: "Mince", Quantity: 400, UnitID: "g", UnitName: "g"},
		{FoodID: "pasta", Name: "Pasta", Quantity: 500, UnitID: "g", UnitName: "g"},
		{FoodID: "cream", Name: "Cream", Quantity: 2, UnitID: "dl", UnitName: "dl"},
		{FoodID: "salt", Name: "Salt"},
		{FoodID: "egg", Name: "Egg", Quantity: 1},
		{Name: "water"},
	}}
	res, err := s.ApplyCooked(ctx, meal)
	if err != nil {
		t.Fatal(err)
	}
	if res.Consumed != 3 || res.Reviews != 4 {
		t.Fatalf("result = %+v", res)
	}
	if got := quantities(t, s, Fridge); !equal(got, map[string]float64{"Egg||": 1}) {
		t.Fatalf("fridge = %v", got)
	}
	if got := quantities(t, s, Freezer); got["Mince|g|"] != 400 {
		t.Fatalf("freezer = %v", got)
	}
	reviews, _ := s.Reviews(ctx)
	reasons := map[string]string{}
	for _, r := range reviews {
		reasons[r.Name] = r.Reason
	}
	want := map[string]string{
		"Pasta": "unit mismatch: stock is in packet", "Cream": "short by 1 dl",
		"Salt": "not in stock", "water": "ingredient is not linked to a Mealie food",
	}
	for name, reason := range want {
		if reasons[name] != reason {
			t.Errorf("review %s = %q; want %q", name, reasons[name], reason)
		}
	}
	if _, err := s.ApplyCooked(ctx, meal); !errors.Is(err, ErrAlreadyCooked) {
		t.Fatalf("second apply = %v", err)
	}
	if err := s.Undo(ctx, res.OpID); err != nil {
		t.Fatal(err)
	}
	if got := quantities(t, s, Fridge); got["Mince|g|"] != 300 || got["Cream|dl|"] != 1 || got["Egg||"] != 2 {
		t.Fatalf("fridge after undo = %v", got)
	}
	if reviews, _ := s.Reviews(ctx); len(reviews) != 0 {
		t.Fatalf("reviews after undo = %v", reviews)
	}
	if cooked, _ := s.Cooked(ctx); len(cooked) != 0 {
		t.Fatalf("cooked after undo = %v", cooked)
	}
	if _, err := s.ApplyCooked(ctx, meal); err != nil {
		t.Fatalf("reapply after undo = %v", err)
	}
}

func equal(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
