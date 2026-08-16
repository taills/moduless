// Command inventory is an example plugin built around a transaction.
//
// The other three examples cover a destination (notes), a gate (ratelimit) and
// a recorder (audit). This one is about the case where a plugin cannot get the
// right answer without one: reserving stock. Reading a level, deciding, and
// writing the decrement have to be one indivisible step, or two customers both
// see the last unit and both get it.
//
//	CGO_ENABLED=0 go build -o inventory/bin/plugin ./extension-example/inventory
//	cp extension-example/inventory/manifest.yaml inventory/
//	PLUGIN_DIR=$(pwd) go run ./core
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// Item is a stock level. Version is not stored: Core keeps it alongside the
// document and hands it back from Get.
type Item struct {
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	OnHand   int    `json:"on_hand"`
	Reserved int    `json:"reserved"`
}

// Reservation records that stock was taken out of circulation.
type Reservation struct {
	ID      string `json:"id"`
	SKU     string `json:"sku"`
	Qty     int    `json:"qty"`
	For     string `json:"for"`
	Created string `json:"created"`
}

// settings is what an operator configures. Guarded because Core pushes changes
// on a background goroutine while requests are being served.
var settings struct {
	sync.RWMutex
	lowStockAt int
	alertToken string
}

func lowStockAt() int {
	settings.RLock()
	defer settings.RUnlock()
	return settings.lowStockAt
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", listItems)
	mux.HandleFunc("PUT /items/{sku}", putItem)
	mux.HandleFunc("POST /items/{sku}/reserve", reserve)
	mux.HandleFunc("POST /items/{sku}/restock", restock)

	log.SetPrefix("[inventory] ")

	sdk.Serve(sdk.Config{
		Handler: mux,
		// Called once at start-up with whatever the operator has set, and
		// again on every change. One code path, so there is no chance of the
		// start-up read and the update disagreeing.
		OnConfigChanged: func(cfg map[string]string) {
			settings.Lock()
			defer settings.Unlock()
			// Values are always strings. Parse, and fall back rather than
			// disable the feature: a typo in the console should not silently
			// turn off low-stock alerting.
			if n, err := strconv.Atoi(cfg["low_stock_at"]); err == nil && n >= 0 {
				settings.lowStockAt = n
			} else if settings.lowStockAt == 0 {
				settings.lowStockAt = 5
			}
			settings.alertToken = cfg["alert_token"]
		},
	})
}

// reserve takes stock, or refuses.
//
// The whole handler is one transaction because the decision and the write have
// to be the same event. Without it: two requests read on_hand=1, both conclude
// there is enough, and both write on_hand=0 — one unit sold twice, and neither
// write failed.
func reserve(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")

	var req struct {
		Qty int    `json:"qty"`
		For string `json:"for"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Qty <= 0 {
		http.Error(w, "qty must be positive", http.StatusBadRequest)
		return
	}

	var (
		taken     Reservation
		remaining int
		err       error
	)
	// Retried, because under contention both of these are expected outcomes
	// rather than failures:
	//
	//   ErrVersionConflict  someone else wrote this item first. A transaction
	//                       makes the steps atomic, it does not make them
	//                       uncontended — the loser is refused rather than
	//                       allowed to overwrite.
	//   ErrRateLimited      the plugin is at its concurrent-transaction
	//                       ceiling. A transaction holds a database
	//                       connection, so Core caps how many one plugin may
	//                       hold; a busy moment is not a fault.
	//
	// This is the part that is easy to get wrong, and this example got it
	// wrong first — twice. Thirty concurrent reservations against ten units
	// gave 2 sold and 28 server errors without the conflict retry, and 10 sold
	// with 20 server errors once only conflicts were retried. With both, it
	// sells exactly ten and tells the other twenty there is nothing left.
	for attempt := range maxReserveAttempts {
		err = reserveOnce(r, sku, req.Qty, req.For, &taken, &remaining)
		if !errors.Is(err, sdk.ErrVersionConflict) && !errors.Is(err, sdk.ErrRateLimited) {
			break
		}
		if attempt < maxReserveAttempts-1 {
			// Backing off at all matters more than the exact shape: without a
			// pause the retries collide again immediately.
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
		}
	}
	if errors.Is(err, sdk.ErrVersionConflict) || errors.Is(err, sdk.ErrRateLimited) {
		http.Error(w, "inventory is busy, try again", http.StatusServiceUnavailable)
		return
	}

	switch {
	case errors.Is(err, errNoSuchItem):
		http.Error(w, "no such item", http.StatusNotFound)
		return
	case errors.Is(err, errNotEnough):
		// 409: the request was well formed and the world said no. Retrying it
		// unchanged may succeed once stock arrives.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		log.Printf("reserve %s: %v", sku, err)
		http.Error(w, "could not reserve", http.StatusInternalServerError)
		return
	}

	// Only after the transaction commits. An alert fired inside it would go
	// out for a reservation that then rolled back, and there is no unsending.
	if remaining <= lowStockAt() {
		log.Printf("low stock: %s down to %d", sku, remaining)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"reservation": taken,
		"remaining":   remaining,
	})
}

// maxReserveAttempts bounds the retry, so a permanently hot item answers
// instead of holding a connection open forever.
const maxReserveAttempts = 8

// runTx is the seam over the transaction.
//
// A function that calls sdk.DB.Tx, not the method value `sdk.DB.Tx` itself. The
// host clients are nil until Core hands over the reverse connection in
// Configure, so a method value captured at package initialisation would bind
// that nil receiver permanently and panic on the first reservation in
// production — while looking perfectly correct. It is the same timing trap as
// reading configuration in main(): the thing is not there yet.
//
// The callback takes sdk.TxOps rather than *sdk.TxClient, which is what makes
// the body below reachable from a test: TxClient holds an unexported gRPC
// client, so no test can build one.
var runTx = func(ctx context.Context, timeout time.Duration, fn func(sdk.TxOps) error) error {
	return sdk.DB.Tx(ctx, timeout, fn)
}

// reserveOnce is one attempt: read the level, decide, write both documents.
func reserveOnce(r *http.Request, sku string, qty int, forWhom string,
	taken *Reservation, remaining *int) error {

	return runTx(r.Context(), 10*time.Second, func(tx sdk.TxOps) error {
		var item Item
		// The version comes back from a transactional Get for the same reason
		// it does outside one: two transactions can both read this row, and
		// the second write proceeds against a row the first already changed.
		found, version, err := tx.Get(r.Context(), "items", sku, &item)
		if err != nil {
			return fmt.Errorf("read %s: %w", sku, err)
		}
		if !found {
			return errNoSuchItem
		}
		available := item.OnHand - item.Reserved
		if available < qty {
			return fmt.Errorf("%w: %d available, %d requested", errNotEnough, available, qty)
		}

		item.Reserved += qty
		if _, err := tx.PutIfVersion(r.Context(), "items", sku, item, version); err != nil {
			return fmt.Errorf("update %s: %w", sku, err)
		}

		*taken = Reservation{
			ID:      sdk.NewID(),
			SKU:     sku,
			Qty:     qty,
			For:     forWhom,
			Created: time.Now().UTC().Format(time.RFC3339),
		}
		if _, err := tx.Put(r.Context(), "reservations", taken.ID, *taken); err != nil {
			return fmt.Errorf("write reservation: %w", err)
		}

		// Read back inside the transaction. The reservation above is not
		// committed yet, and a query that ran outside this transaction would
		// not see it — it would report a stale count and nothing would say so.
		*remaining = item.OnHand - item.Reserved
		return nil
	})
}

var (
	errNoSuchItem = errors.New("no such item")
	errNotEnough  = errors.New("not enough stock")
)

// restock adds stock. No transaction: it is a single read-modify-write on one
// document, which optimistic locking already makes safe. Reach for a
// transaction when two documents have to change together, not by reflex.
func restock(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")

	var req struct {
		Qty int `json:"qty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Qty <= 0 {
		http.Error(w, "qty must be a positive integer", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// Retried because a losing write is the expected outcome under
	// contention, not a failure. Bounded, so a permanently hot document
	// answers instead of spinning.
	for attempt := range 3 {
		var item Item
		found, version, err := sdk.DB.Get(ctx, "items", sku, &item)
		if err != nil {
			http.Error(w, "could not read item", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such item", http.StatusNotFound)
			return
		}

		item.OnHand += req.Qty
		_, err = sdk.DB.PutIfVersion(ctx, "items", sku, item, version)
		if err == nil {
			writeJSON(w, http.StatusOK, item)
			return
		}
		if !errors.Is(err, sdk.ErrVersionConflict) {
			log.Printf("restock %s: %v", sku, err)
			http.Error(w, "could not restock", http.StatusInternalServerError)
			return
		}
		if attempt == 2 {
			log.Printf("restock %s: giving up after 3 attempts: %v", sku, err)
			http.Error(w, "item is being updated too often, try again", http.StatusConflict)
			return
		}
	}
}

func listItems(w http.ResponseWriter, r *http.Request) {
	var items []Item
	q := sdk.DB.Where("items").Sort("sku").Limit(100)
	if after := r.URL.Query().Get("after"); after != "" {
		q = q.After(after)
	}
	next, err := q.All(r.Context(), &items)
	if err != nil {
		http.Error(w, "could not list items", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

func putItem(w http.ResponseWriter, r *http.Request) {
	sku := r.PathValue("sku")

	var item Item
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&item); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	item.SKU = sku

	if _, err := sdk.DB.Put(r.Context(), "items", sku, item); err != nil {
		http.Error(w, "could not save item", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
