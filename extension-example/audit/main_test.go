package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// Most of this example was already covered end to end: tests/audit_blindspot_test.go
// proves an ordinary request and an oversized one both leave a record, and
// tests/six_plugins_test.go proves the identity another plugin established
// reaches it. Those are stronger than anything here could be — a real Core, a
// real pipeline, a real refusal.
//
// What had no coverage at all is everything around that: the retention setting,
// the cutoff the purge deletes against, and who may read the trail. The purge is
// the one that matters most, because it deletes the record of what happened and
// nothing gets it back.

func query(raw string) url.Values {
	v, err := url.ParseQuery(raw)
	if err != nil {
		panic(err)
	}
	return v
}

// --- who may read the trail ---------------------------------------------

// The comment on listEntries says a menu's roles: [admin] does not protect the
// route, only the menu item. This is that claim, pinned — it is exactly the
// check someone removes while "simplifying", and the failure is silent: the
// audit trail simply becomes readable by everyone.
func TestOnlyAnAdminMayReadTheTrail(t *testing.T) {
	tests := []struct {
		name string
		user *sdk.UserContext
		want int
	}{
		{name: "anonymous", want: http.StatusForbidden},
		{name: "a signed-in non-admin", user: &sdk.UserContext{UserID: "7", Username: "ann",
			Roles: []string{"reader"}}, want: http.StatusForbidden},
		{name: "an admin", user: &sdk.UserContext{UserID: "1", Username: "root",
			Roles: []string{"admin"}}, want: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/entries", nil)
			if tc.user != nil {
				req = req.WithContext(sdk.WithUser(req.Context(), tc.user))
			}
			w := httptest.NewRecorder()

			listEntries(w, req)

			// The admin case reaches the store, which is not there under go
			// test, so 500 is how far it gets — and that is the point: it got
			// past the guard the other two did not.
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// --- the purge cutoff ---------------------------------------------------

// The boundary is computed from the occurrence the run is for, not from when it
// actually ran. A job delayed by an hour must delete the same rows it would
// have deleted on time.
func TestTheCutoffComesFromTheScheduledTimeNotTheClock(t *testing.T) {
	scheduled := time.Date(2026, 3, 15, 4, 23, 0, 0, time.UTC)

	got := expiredQuery(scheduled.Unix(), 30).Describe()

	if len(got.Filters) != 1 {
		t.Fatalf("filters = %+v, want one cutoff", got.Filters)
	}
	f := got.Filters[0]
	if f.Field != "at" || f.Op != "LT" {
		t.Fatalf("filter = %+v, want at LT", f)
	}
	want := "2026-02-13T04:23:00Z" // thirty days before, to the second
	if f.Values[0] != want {
		t.Errorf("cutoff = %q, want %q", f.Values[0], want)
	}
	// A page size, or one run of a long-neglected trail tries to read the whole
	// table into memory.
	if got.Limit != 200 {
		t.Errorf("limit = %d, want a bounded page", got.Limit)
	}
}

func TestTheRetentionWindowMovesTheCutoff(t *testing.T) {
	scheduled := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC).Unix()

	sevenDays := expiredQuery(scheduled, 7).Describe().Filters[0].Values[0]
	ninetyDays := expiredQuery(scheduled, 90).Describe().Filters[0].Values[0]

	if sevenDays != "2026-03-08T00:00:00Z" {
		t.Errorf("7-day cutoff = %q", sevenDays)
	}
	if ninetyDays != "2025-12-15T00:00:00Z" {
		t.Errorf("90-day cutoff = %q", ninetyDays)
	}
	// Lexical order has to equal chronological order, or Lt on a string column
	// deletes the wrong side of the boundary.
	if !(ninetyDays < sevenDays) {
		t.Errorf("%q is not lexically before %q; RFC3339 is used precisely so "+
			"that string comparison is time comparison", ninetyDays, sevenDays)
	}
}

// --- the retention setting ----------------------------------------------

// A typo in the console must not switch the purge off. Falling back to the
// default keeps the trail bounded; treating an unparseable value as "keep
// forever" is how a disk fills up quietly.
func TestAnUnusableRetentionFallsBackRatherThanDisablingThePurge(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]string
		want int
	}{
		{name: "an admin's value", cfg: map[string]string{"retention_days": "7"}, want: 7},
		{name: "not a number", cfg: map[string]string{"retention_days": "thirty"}, want: defaultRetentionDays},
		{name: "zero", cfg: map[string]string{"retention_days": "0"}, want: defaultRetentionDays},
		{name: "negative", cfg: map[string]string{"retention_days": "-5"}, want: defaultRetentionDays},
		{name: "empty", cfg: map[string]string{"retention_days": ""}, want: defaultRetentionDays},
		{name: "absent", cfg: map[string]string{}, want: defaultRetentionDays},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			onConfigChanged(tc.cfg)

			cfgMu.RLock()
			got := retentionDays
			cfgMu.RUnlock()

			if got != tc.want {
				t.Errorf("retentionDays = %d, want %d", got, tc.want)
			}
		})
	}
	t.Cleanup(func() { onConfigChanged(map[string]string{}) })
}

// --- the operator's listing ---------------------------------------------

func TestTheListingIsNewestFirstAndBounded(t *testing.T) {
	got := entriesQuery(query("")).Describe()

	if len(got.Sort) != 1 || got.Sort[0].Field != "at" || !got.Sort[0].Descending {
		t.Errorf("sort = %+v, want at descending", got.Sort)
	}
	if got.Limit != defaultPageLimit {
		t.Errorf("limit = %d, want %d", got.Limit, defaultPageLimit)
	}
	if len(got.Filters) != 0 {
		t.Errorf("an unfiltered listing carries %+v", got.Filters)
	}
}

func TestTheUserFilterAndCursorReachTheQuery(t *testing.T) {
	got := entriesQuery(query("user=ann&cursor=entry-9")).Describe()

	if len(got.Filters) != 1 || got.Filters[0].Field != "user" ||
		got.Filters[0].Op != "EQ" || got.Filters[0].Values[0] != "ann" {
		t.Errorf("filters = %+v, want user EQ ann", got.Filters)
	}
	if got.Cursor != "entry-9" {
		t.Errorf("cursor = %q; without it the second page is the first page", got.Cursor)
	}
}

func TestThePageLimitIsClamped(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", defaultPageLimit},
		{"25", 25},
		{"200", maxPageLimit},
		{"201", defaultPageLimit}, // above the ceiling, not clamped to it
		{"0", defaultPageLimit},   // would read as an empty audit trail
		{"-1", defaultPageLimit},
		{"lots", defaultPageLimit},
	}
	for _, tc := range tests {
		if got := pageLimit(tc.raw); got != tc.want {
			t.Errorf("pageLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// The record itself: a request with no caller must not take the plugin down.
// sdk.User returns nil for an anonymous request, and a panic in a filter kills
// the process rather than failing the one request — so one unauthenticated call
// would stop auditing for everybody.
func TestAnAnonymousRequestIsRecordedWithoutPanicking(t *testing.T) {
	res, err := recordWrite(context.Background(), &sdk.FilterRequest{
		Phase: sdk.PhaseLog, Method: http.MethodPost,
		Path: "/api/plugins/notes/items", ResponseStatus: 201,
	})
	if err != nil {
		t.Fatalf("recordWrite: %v", err)
	}
	// The log phase's answer is ignored, but it still has to be a valid one.
	if got := res.Inspect(); got.Action != sdk.ActionContinue {
		t.Errorf("action = %v, want Continue", got.Action)
	}
}
