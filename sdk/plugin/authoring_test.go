package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// What a plugin author can test without a live Core.
//
// This matters more than most SDK tests: an external team writing a plugin has
// only what this package exports, and an agent given nothing but the plugin
// guide reported that there was no testing story at all. Two things were
// actually impossible rather than merely undocumented — asserting what a filter
// decided, and giving a handler an authenticated caller — and both are the
// exact things the guide tells authors to get right.
//
// These tests are written the way a plugin author would write theirs, using
// only exported API, so they fail if that surface stops being enough.

// A filter is an ordinary function. Calling it is easy; the problem was
// checking the answer.
func TestAuthorCanAssertWhatAFilterDecided(t *testing.T) {
	guard := func(_ context.Context, req *FilterRequest) (*FilterResult, error) {
		if req.Header.Get("X-Api-Key") == "" {
			return Stop(http.StatusUnauthorized, []byte(`{"error":"no key"}`)).
				WithHeader("WWW-Authenticate", "Bearer"), nil
		}
		return Mutate().SetIdentity(&UserContext{
			UserID: "42", Username: "svc", Roles: []string{"reader"},
		}).SetValue("via", "apikey"), nil
	}

	t.Run("refuses an anonymous request", func(t *testing.T) {
		res, err := guard(context.Background(), &FilterRequest{
			Phase:  PhaseAuthenticate,
			Method: http.MethodGet,
			Path:   "/api/plugins/notes/items",
			Header: http.Header{},
		})
		if err != nil {
			t.Fatalf("filter: %v", err)
		}

		got := res.Inspect()
		if got.Action != ActionStop {
			t.Fatalf("action = %s, want stop", got.Action)
		}
		if got.Status != http.StatusUnauthorized {
			t.Errorf("status = %d", got.Status)
		}
		if got.Header.Get("WWW-Authenticate") == "" {
			t.Error("no challenge header on a 401")
		}
	})

	t.Run("grants an identity to a request that carries a key", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Api-Key", "abc")
		res, err := guard(context.Background(), &FilterRequest{
			Phase: PhaseAuthenticate, Method: http.MethodGet, Path: "/x", Header: h,
		})
		if err != nil {
			t.Fatalf("filter: %v", err)
		}

		got := res.Inspect()
		if got.Action != ActionMutate {
			t.Fatalf("action = %s, want mutate", got.Action)
		}
		// The roles matter as much as the id: everything downstream reads them.
		if !got.Identity.HasRole("reader") {
			t.Errorf("identity = %+v; the roles did not survive", got.Identity)
		}
		if got.Values["via"] != "apikey" {
			t.Errorf("values = %v", got.Values)
		}
	})
}

// The authorization pattern the guide teaches, tested the way an author would.
func TestAuthorCanTestAHandlerThatChecksRoles(t *testing.T) {
	// Exactly what docs/plugin-development.md shows.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !User(r.Context()).HasRole("admin") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": User(r.Context()).Name()})
	})

	for _, tc := range []struct {
		name string
		user *UserContext
		want int
	}{
		// Nil rather than absent: an anonymous request is what Core passes, and
		// the accessors are nil-safe precisely so this does not panic. A panic
		// in a plugin kills the process, so this case is the one that matters.
		{"anonymous", nil, http.StatusForbidden},
		{"an ordinary user", &UserContext{UserID: "7", Username: "bob", Roles: []string{"user"}}, http.StatusForbidden},
		{"an admin", &UserContext{UserID: "1", Username: "root", Roles: []string{"admin"}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/keys", nil)
			req = req.WithContext(WithUser(req.Context(), tc.user))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A filter that returns Continue says so, rather than reading as an
// unrecognised answer. Worth its own case: Continue is the zero value of the
// underlying action, and a decision type that could not tell "continue" from
// "nothing was set" would be useless for the most common outcome.
func TestContinueIsDistinctFromNothing(t *testing.T) {
	if got := Continue().Inspect().Action; got != ActionContinue {
		t.Errorf("action = %s, want continue", got)
	}
	var none *FilterResult
	if got := none.Inspect().Action; got != ActionUnrecognised {
		t.Errorf("a nil result reads as %s; a filter that returned nothing is not "+
			"the same as one that let the request through", got)
	}
}
