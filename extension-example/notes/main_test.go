package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// This example uses the widest slice of the host surface — documents, queue,
// events and a cron job — and most of it needs no Core to test. What does is
// the query: it cannot be executed here, but Describe() reports what was built,
// which is where the decisions live.

func listRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/notes?"+query, nil)
}

func TestTheListingAsksForTheNewestPageFirst(t *testing.T) {
	got := notesQuery(listRequest("")).Describe()

	if got.Collection != "notes" {
		t.Errorf("collection = %q", got.Collection)
	}
	if len(got.Filters) != 0 {
		t.Errorf("an unfiltered listing carries %d filters: %+v", len(got.Filters), got.Filters)
	}
	if len(got.Sort) != 1 || got.Sort[0].Field != "created" || !got.Sort[0].Descending {
		t.Errorf("sort = %+v, want created descending: an unsorted listing pages "+
			"through rows in whatever order the store returns them", got.Sort)
	}
	// Without a limit the first request for a large collection returns all of
	// it, which is a page size decided by whoever wrote the most notes.
	if got.Limit != 50 {
		t.Errorf("limit = %d, want 50", got.Limit)
	}
	if got.Cursor != "" {
		t.Errorf("cursor = %q on a first page", got.Cursor)
	}
}

func TestTheAuthorFilterReachesTheQuery(t *testing.T) {
	got := notesQuery(listRequest("author=ann")).Describe()

	if len(got.Filters) != 1 {
		t.Fatalf("filters = %+v, want one", got.Filters)
	}
	f := got.Filters[0]
	if f.Field != "author" || f.Op != "EQ" || len(f.Values) != 1 || f.Values[0] != "ann" {
		t.Errorf("filter = %+v, want author EQ ann", f)
	}
	// The other clauses have to survive the filter being added.
	if got.Limit != 50 || len(got.Sort) != 1 {
		t.Errorf("adding a filter lost the sort or the limit: %+v", got)
	}
}

// An empty parameter is not a filter on the empty string. Treating ?author= as
// Eq("author", "") returns nothing and looks like a broken listing.
func TestAnEmptyAuthorIsNotAFilter(t *testing.T) {
	if got := notesQuery(listRequest("author=")).Describe(); len(got.Filters) != 0 {
		t.Errorf("filters = %+v, want none for an empty parameter", got.Filters)
	}
}

func TestTheCursorIsCarriedIntoTheNextPage(t *testing.T) {
	got := notesQuery(listRequest("after=note-123&author=ann")).Describe()

	if got.Cursor != "note-123" {
		t.Errorf("cursor = %q; without it every page is the first page and the "+
			"caller loops forever", got.Cursor)
	}
	if len(got.Filters) != 1 {
		t.Errorf("the filter was lost when paging: %+v", got.Filters)
	}
}

// --- what needs no query ------------------------------------------------

func TestCreateRefusesANoteWithNoTitle(t *testing.T) {
	body := bytes.NewBufferString(`{"body":"text with no title"}`)
	req := httptest.NewRequest(http.MethodPost, "/notes", body)
	w := httptest.NewRecorder()

	createNote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	// The refusal has to reach the caller as something readable.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Errorf("the 400 body is not JSON: %s", w.Body.String())
	}
}

func TestCreateRefusesABodyThatIsNotJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/notes", bytes.NewBufferString(`not json`))
	w := httptest.NewRecorder()

	createNote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 rather than a 500 blaming the store", w.Code)
	}
}

// Validation runs before the store is touched, which is what makes the case
// above reachable without a Core. Past it, an unbound capability now reports
// itself instead of taking the process down: sdk.DB returns ErrHostUnavailable
// where it used to dereference a nil client.
func TestAValidNoteReachesTheStoreAndSaysSoWhenItIsNotThere(t *testing.T) {
	body := bytes.NewBufferString(`{"title":"a title","body":"text"}`)
	req := httptest.NewRequest(http.MethodPost, "/notes", body)
	w := httptest.NewRecorder()

	createNote(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: there is no store behind this test", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("host capabilities")) {
		t.Errorf("the failure does not say why: %s", w.Body.String())
	}
}

// The same guard, stated directly: building a query outside a Core works,
// running one does not, and the difference is a sentence rather than a crash.
func TestAQueryBuildsWithoutACoreAndRefusesToRun(t *testing.T) {
	q := notesQuery(listRequest("author=ann"))

	if got := q.Describe(); len(got.Filters) != 1 {
		t.Fatalf("the query did not build: %+v", got)
	}
	if _, err := q.Count(t.Context()); !errors.Is(err, sdk.ErrHostUnavailable) {
		t.Errorf("err = %v, want ErrHostUnavailable", err)
	}
	var notes []Note
	if _, err := q.All(t.Context(), &notes); !errors.Is(err, sdk.ErrHostUnavailable) {
		t.Errorf("err = %v, want ErrHostUnavailable", err)
	}
}
