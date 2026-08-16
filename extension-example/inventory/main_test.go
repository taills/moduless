package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// The transaction body is where this example's invariants live — read the
// level, refuse when short, take stock and record the reservation together —
// and none of it was reachable from a test until sdk.DB.Tx started handing the
// callback an interface. TxClient holds an unexported gRPC client, so a
// callback typed to it can only be run by a live Core.

// --- a transaction that records what happened ---------------------------

type write struct {
	collection string
	id         string
	value      any
	expected   int64 // -1 for an unconditional Put
}

type fakeTx struct {
	docs     map[string]any
	versions map[string]int64
	writes   []write

	// conflictOn makes PutIfVersion fail once for this collection, the way a
	// concurrent writer would.
	conflictOn string
}

func newTx() *fakeTx {
	return &fakeTx{docs: map[string]any{}, versions: map[string]int64{}}
}

func (f *fakeTx) key(collection, id string) string { return collection + "/" + id }

func (f *fakeTx) Get(_ context.Context, collection, id string, dest any) (bool, int64, error) {
	doc, ok := f.docs[f.key(collection, id)]
	if !ok {
		return false, 0, nil
	}
	// Round-trip through JSON so dest is populated the way the real store does
	// it, rather than by sharing a pointer the caller could mutate.
	raw, err := json.Marshal(doc)
	if err != nil {
		return false, 0, err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, 0, err
	}
	return true, f.versions[f.key(collection, id)], nil
}

func (f *fakeTx) Put(_ context.Context, collection, id string, value any) (int64, error) {
	f.writes = append(f.writes, write{collection, id, value, -1})
	f.docs[f.key(collection, id)] = value
	f.versions[f.key(collection, id)]++
	return f.versions[f.key(collection, id)], nil
}

func (f *fakeTx) PutIfVersion(_ context.Context, collection, id string, value any, expected int64) (int64, error) {
	if f.conflictOn == collection {
		f.conflictOn = "" // once, so a retry can succeed
		return 0, sdk.ErrVersionConflict
	}
	if got := f.versions[f.key(collection, id)]; got != expected {
		return 0, sdk.ErrVersionConflict
	}
	f.writes = append(f.writes, write{collection, id, value, expected})
	f.docs[f.key(collection, id)] = value
	f.versions[f.key(collection, id)]++
	return f.versions[f.key(collection, id)], nil
}

func (f *fakeTx) Delete(_ context.Context, collection, id string) error {
	delete(f.docs, f.key(collection, id))
	return nil
}

func (f *fakeTx) stock(sku string, item Item, version int64) {
	f.docs[f.key("items", sku)] = item
	f.versions[f.key("items", sku)] = version
}

// withTx points the seam at a fake for one test.
func withTx(t *testing.T, f *fakeTx) {
	t.Helper()
	prev := runTx
	runTx = func(ctx context.Context, _ time.Duration, fn func(sdk.TxOps) error) error {
		return fn(f)
	}
	t.Cleanup(func() { runTx = prev })
}

func attemptReserve(t *testing.T, sku string, qty int) (Reservation, int, error) {
	t.Helper()
	var taken Reservation
	var remaining int
	err := reserveOnce(httptest.NewRequest("POST", "/reserve", nil), sku, qty, "ops", &taken, &remaining)
	return taken, remaining, err
}

// --- the invariants -----------------------------------------------------

func TestAReservationTakesStockAndRecordsItself(t *testing.T) {
	f := newTx()
	f.stock("widget", Item{SKU: "widget", Name: "Widget", OnHand: 10, Reserved: 2}, 7)
	withTx(t, f)

	taken, remaining, err := attemptReserve(t, "widget", 3)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Both documents, or the stock is taken and nothing records why.
	if len(f.writes) != 2 {
		t.Fatalf("wrote %d documents, want the item and the reservation: %+v", len(f.writes), f.writes)
	}

	item, ok := f.writes[0].value.(Item)
	if !ok {
		t.Fatalf("the first write was not the item: %+v", f.writes[0])
	}
	if item.Reserved != 5 {
		t.Errorf("reserved = %d, want 2 + 3", item.Reserved)
	}
	if item.OnHand != 10 {
		t.Errorf("on_hand = %d; reserving must not consume stock, only hold it", item.OnHand)
	}
	// The version read inside the transaction is the one written back, which is
	// what makes a concurrent reservation lose rather than overwrite.
	if f.writes[0].expected != 7 {
		t.Errorf("wrote against version %d, want the 7 that was read", f.writes[0].expected)
	}

	if taken.SKU != "widget" || taken.Qty != 3 || taken.For != "ops" {
		t.Errorf("reservation = %+v", taken)
	}
	if taken.ID == "" {
		t.Error("the reservation has no id, so nothing can cancel it later")
	}

	// Computed inside the transaction, from the value just written. A count
	// read outside would not see the uncommitted reservation and would be
	// stale with nothing to say so.
	if remaining != 5 {
		t.Errorf("remaining = %d, want 10 on hand less 5 now reserved", remaining)
	}
}

func TestReservingMoreThanIsAvailableIsRefusedAndWritesNothing(t *testing.T) {
	f := newTx()
	// Eight on hand, six already spoken for: two available.
	f.stock("widget", Item{SKU: "widget", OnHand: 8, Reserved: 6}, 1)
	withTx(t, f)

	_, _, err := attemptReserve(t, "widget", 3)
	if !errors.Is(err, errNotEnough) {
		t.Fatalf("err = %v, want errNotEnough", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("a refused reservation wrote %d documents: %+v", len(f.writes), f.writes)
	}
}

// The boundary: exactly what is available must succeed. An off-by-one here is
// the difference between selling the last unit and never selling it.
func TestTakingExactlyWhatIsAvailableSucceeds(t *testing.T) {
	f := newTx()
	f.stock("widget", Item{SKU: "widget", OnHand: 8, Reserved: 6}, 1)
	withTx(t, f)

	_, remaining, err := attemptReserve(t, "widget", 2)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
}

func TestAnUnknownSKUIsRefused(t *testing.T) {
	f := newTx()
	withTx(t, f)

	if _, _, err := attemptReserve(t, "ghost", 1); !errors.Is(err, errNoSuchItem) {
		t.Errorf("err = %v, want errNoSuchItem", err)
	}
}

// A losing writer must report the conflict rather than the reservation, so the
// caller's retry loop can run. Swallowing it would hand back a reservation for
// stock somebody else took.
func TestAVersionConflictIsReported(t *testing.T) {
	f := newTx()
	f.stock("widget", Item{SKU: "widget", OnHand: 10}, 1)
	f.conflictOn = "items"
	withTx(t, f)

	_, _, err := attemptReserve(t, "widget", 1)
	if !errors.Is(err, sdk.ErrVersionConflict) {
		t.Fatalf("err = %v, want a version conflict the retry loop can recognise", err)
	}
	// The reservation must not have been written on the losing attempt.
	for _, w := range f.writes {
		if w.collection == "reservations" {
			t.Error("a reservation was recorded for stock that was not taken")
		}
	}
}

// The claim in the package comment, checked by the compiler: the SDK's own
// transaction client satisfies the interface the callback takes, so nothing
// about production changed when this became testable.
var _ sdk.TxOps = (*sdk.TxClient)(nil)
