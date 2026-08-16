package main

import (
	"context"
	"net/http"
	"testing"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// The docs say a filter is an ordinary Go function and tests like one, with no
// Core running. This is that claim, exercised on the example that has the least
// standing in its way: ratelimit touches no host capability at all.

func TestBurstIsSpentThenRequestsAreRefused(t *testing.T) {
	lim := newLimiter()
	lim.configure(map[string]string{"requests_per_minute": "60", "burst": "3"})

	req := &sdk.FilterRequest{
		Phase:    sdk.PhasePreRoute,
		Method:   http.MethodGet,
		Path:     "/api/plugins/notes/items",
		ClientIP: "203.0.113.7",
	}

	for i := range 3 {
		res, err := lim.check(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if got := res.Inspect(); got.Action != sdk.ActionContinue {
			t.Fatalf("call %d was refused with a burst of 3: %+v", i+1, got)
		}
	}

	res, err := lim.check(context.Background(), req)
	if err != nil {
		t.Fatalf("fourth call: %v", err)
	}
	got := res.Inspect()
	if got.Action != sdk.ActionStop {
		t.Fatalf("the fourth call was allowed on a burst of 3: %+v", got)
	}
	if got.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", got.Status)
	}
	if got.Header.Get("Retry-After") == "" {
		t.Error("a refusal with no Retry-After tells the caller nothing about when to come back")
	}
}
