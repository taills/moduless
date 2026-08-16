package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// The two traps this example documents are the two worth pinning: a short
// circuit carries only its own headers, and returning Stop when nothing changed
// is neither free nor harmless.

func jsonResponse(body string) *sdk.FilterRequest {
	return &sdk.FilterRequest{
		Phase:          sdk.PhasePostHandler,
		Method:         http.MethodGet,
		Path:           "/api/plugins/notes/items",
		ResponseHeader: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		ResponseBody:   []byte(body),
	}
}

func TestConfiguredFieldsAreMaskedAtAnyDepth(t *testing.T) {
	configure(map[string]string{"fields": "Password, ssn", "mask": "***"})

	req := jsonResponse(`{"user":{"name":"ann","password":"hunter2"},"rows":[{"ssn":"111"}]}`)
	res, err := redact(context.Background(), req)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	got := res.Inspect()
	if got.Action != sdk.ActionStop {
		t.Fatalf("a response containing password was passed through: %+v", got)
	}

	var doc struct {
		User struct{ Name, Password string } `json:"user"`
		Rows []struct{ SSN string }          `json:"rows"`
	}
	if err := json.Unmarshal(got.Body, &doc); err != nil {
		t.Fatalf("the replacement is not valid JSON: %v", err)
	}
	// Field names are matched case-insensitively, so "Password" in the config
	// covers "password" on the wire.
	if doc.User.Password != "***" {
		t.Errorf("password = %q, want the mask", doc.User.Password)
	}
	if len(doc.Rows) != 1 || doc.Rows[0].SSN != "***" {
		t.Errorf("a field inside an array was missed: %+v", doc.Rows)
	}
	if doc.User.Name != "ann" {
		t.Errorf("an unconfigured field was changed: %q", doc.User.Name)
	}
}

// Trap one. Core writes the short circuit's headers and drops whatever the
// backend set, so a replacement that does not carry Content-Type across strips
// it from every response this plugin touches — and the symptom, a browser
// showing JSON as plain text, points nowhere near here.
func TestTheReplacementKeepsTheBackendsContentType(t *testing.T) {
	configure(map[string]string{"fields": "password"})

	res, err := redact(context.Background(), jsonResponse(`{"password":"hunter2"}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	got := res.Inspect()
	if ct := got.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q; the backend's headers were dropped by the replacement", ct)
	}
}

// Trap two, and the three pass-through paths. Continue is not just the cheap
// answer here, it is the correct one: Stop makes every response in the system
// pass through this plugin's idea of what a response looks like.
func TestNothingToDoMeansContinue(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]string
		req  *sdk.FilterRequest
	}{
		{
			name: "no fields configured",
			cfg:  map[string]string{"fields": ""},
			req:  jsonResponse(`{"password":"hunter2"}`),
		},
		{
			name: "configured field is absent",
			cfg:  map[string]string{"fields": "password"},
			req:  jsonResponse(`{"name":"ann"}`),
		},
		{
			name: "body is not JSON",
			cfg:  map[string]string{"fields": "password"},
			req: &sdk.FilterRequest{
				Phase:          sdk.PhasePostHandler,
				ResponseHeader: http.Header{"Content-Type": []string{"text/html"}},
				ResponseBody:   []byte("<p>password</p>"),
			},
		},
		{
			// Claims to be JSON and is not. Failing closed here would take down
			// every endpoint that ever returns something unparseable.
			name: "body is malformed JSON",
			cfg:  map[string]string{"fields": "password"},
			req:  jsonResponse(`{"password": `),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configure(tc.cfg)
			res, err := redact(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("redact: %v", err)
			}
			if got := res.Inspect(); got.Action != sdk.ActionContinue {
				t.Errorf("action = %v, want Continue: replacing a response that "+
					"needed no change costs a header copy on every request", got.Action)
			}
		})
	}
}

func TestTheMaskDefaultsWhenUnset(t *testing.T) {
	configure(map[string]string{"fields": "password"})

	res, err := redact(context.Background(), jsonResponse(`{"password":"hunter2"}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	var doc struct{ Password string }
	if err := json.Unmarshal(res.Inspect().Body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Password != "[redacted]" {
		t.Errorf("mask = %q, want the default when the operator set none", doc.Password)
	}
}
