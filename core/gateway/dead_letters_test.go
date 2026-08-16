package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/pluginhost"
)

// The admin routes for the queue's failure sink.
//
// The data layer having Dead and RetryDead is half of it; nothing reaches them
// unless the handler routes to them and main.go passes the store in. That
// join is what this covers, because a capability complete on both ends with
// nothing connecting them is the shape that keeps turning up here.

// plainUser is the counterpart to auth_test's stubResolver, which only ever
// answers admin: these routes have to be checked against somebody who is
// logged in and is not one.
type plainUser struct{}

func (plainUser) Resolve(string) (auth.User, bool) {
	return auth.User{ID: 2, Username: "someone", Role: "user"}, true
}

type quietManager struct{}

func (quietManager) List() []pluginhost.Status             { return nil }
func (quietManager) Scan()                                 {}
func (quietManager) Enable(context.Context, string) error  { return nil }
func (quietManager) Disable(context.Context, string) error { return nil }
func (quietManager) Upgrade(context.Context, string) error { return nil }
func (quietManager) SetConfig(context.Context, string, map[string]string) error {
	return nil
}

// adminRequest is a request that auth_test's stubResolver accepts as admin.
func adminRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer good")
	return r
}

type stubDeadLetters struct {
	messages  []db.DeadMessage
	askedFor  string
	retried   int64
	retryFor  string
	retryFail error
}

func (s *stubDeadLetters) Dead(_ context.Context, key string, _ int) ([]db.DeadMessage, error) {
	s.askedFor = key
	return s.messages, nil
}

func (s *stubDeadLetters) RetryDead(_ context.Context, key string, id int64) error {
	s.retryFor, s.retried = key, id
	return s.retryFail
}

func newDeadLetterHandler(store DeadLetterStore) *PluginsHandler {
	h := NewPluginsHandler(stubResolver{}, quietManager{})
	h.DeadLetters = store
	return h
}

func TestDeadLetterListingIsServed(t *testing.T) {
	store := &stubDeadLetters{messages: []db.DeadMessage{{
		ID: 7, Topic: "accounts", Payload: []byte(`{"account":"a"}`),
		Attempts: 5, LastError: "upstream returned 500", FailedAt: time.Now(),
	}}}
	h := newDeadLetterHandler(store)

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/dead"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if store.askedFor != "syncer" {
		t.Errorf("the store was asked for plugin %q, want %q", store.askedFor, "syncer")
	}

	var got struct {
		Messages []db.DeadMessage `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].ID != 7 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	// The reason and the payload are the whole point of the endpoint: a list
	// of ids would be no more actionable than the count it replaces.
	if got.Messages[0].LastError == "" || len(got.Messages[0].Payload) == 0 {
		t.Errorf("the response dropped the reason or the payload: %+v", got.Messages[0])
	}
}

func TestAnEmptyDeadLetterListIsAnEmptyList(t *testing.T) {
	h := newDeadLetterHandler(&stubDeadLetters{})

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/dead"))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if string(raw["messages"]) != "[]" {
		t.Errorf("messages = %s, want []; null makes a console distinguish two "+
			"cases that mean the same thing", raw["messages"])
	}
}

func TestRetryingADeadLetterIsServed(t *testing.T) {
	store := &stubDeadLetters{}
	h := newDeadLetterHandler(store)

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodPost, PluginsAPIPrefix+"/syncer/dead/7/retry"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if store.retryFor != "syncer" || store.retried != 7 {
		t.Errorf("retried message %d of plugin %q, want 7 of syncer", store.retried, store.retryFor)
	}
}

// A message somebody already retried is a 404, not a 500. The id came from the
// caller and the usual reason it is gone is that the work is already back.
func TestRetryingAMissingDeadLetterIs404(t *testing.T) {
	h := newDeadLetterHandler(&stubDeadLetters{retryFail: errors.New("no dead message 7")})

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodPost, PluginsAPIPrefix+"/syncer/dead/7/retry"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// Without a database the endpoint says so. Reporting an empty list would read
// as "nothing has failed", which is a different and much more comforting claim.
func TestDeadLettersWithoutADatabaseSayASo(t *testing.T) {
	h := NewPluginsHandler(stubResolver{}, quietManager{})

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/dead"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// Every route here runs third-party code's data past an operator, so it is
// admin-only like the rest of this handler.
func TestDeadLettersRequireAnAdmin(t *testing.T) {
	h := NewPluginsHandler(plainUser{}, quietManager{})
	h.DeadLetters = &stubDeadLetters{}

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/dead"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a dead letter carries the plugin's own "+
			"payload, which is not something every logged-in user may read", rec.Code)
	}
}
