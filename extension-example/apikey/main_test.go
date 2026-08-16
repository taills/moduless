package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// This example decides who may reach what, and had no tests.
//
// Most of it needs no seam at all: authorize — the refusal itself — takes a
// context it does not use and touches no host capability, so the security
// decision is an ordinary function. Only authenticate reaches the store, and
// only through resolveKey.
//
// One function is genuinely out of reach: listKeys uses sdk.DB.Where, a
// concrete fluent builder that cannot be substituted without reimplementing it.
// That is 1 of the 14 functions here, which is the honest size of that gap.

func protectedPaths(t *testing.T, prefixes string) {
	t.Helper()
	configure(map[string]string{"protected_prefixes": prefixes})
}

// --- authorize: the refusal ---------------------------------------------

func TestAuthorizeRefusesAnonymousCallersOnProtectedPaths(t *testing.T) {
	protectedPaths(t, "/api/plugins/notes/, /api/system/")

	tests := []struct {
		name     string
		path     string
		identity *sdk.UserContext
		want     sdk.Action
	}{
		{
			name: "anonymous on a protected path is refused",
			path: "/api/plugins/notes/items",
			want: sdk.ActionStop,
		},
		{
			// The comment in the source calls this out: Identity is nil for an
			// anonymous request, and a panic in a filter kills the plugin
			// process rather than failing the one request.
			name: "a nil identity does not panic",
			path: "/api/system/users",
			want: sdk.ActionStop,
		},
		{
			name:     "an authenticated caller passes",
			path:     "/api/plugins/notes/items",
			identity: &sdk.UserContext{UserID: "42", Username: "svc", Roles: []string{"reader"}},
			want:     sdk.ActionContinue,
		},
		{
			name: "an unprotected path is not this filter's business",
			path: "/api/plugins/public/feed",
			want: sdk.ActionContinue,
		},
		{
			// Prefix matching, so everything below a protected prefix is
			// protected too. A rule that only covered exact paths would leave
			// every sub-path open.
			name: "a path below a protected prefix is protected",
			path: "/api/system/users/42/keys",
			want: sdk.ActionStop,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := authorize(context.Background(), &sdk.FilterRequest{
				Phase: sdk.PhaseAuthorize, Method: http.MethodGet,
				Path: tc.path, Identity: tc.identity,
			})
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			got := res.Inspect()
			if got.Action != tc.want {
				t.Fatalf("action = %v, want %v", got.Action, tc.want)
			}
			if tc.want == sdk.ActionStop {
				if got.Status != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", got.Status)
				}
				// Without this header the caller is told no and not told how.
				if got.Header.Get("WWW-Authenticate") == "" {
					t.Error("a 401 with no WWW-Authenticate does not say what would work")
				}
			}
		})
	}
}

// Nothing is protected until an operator says so. A default of "everything is
// protected" would lock the console out of a freshly installed plugin; a
// default of "nothing" is visible in the settings page.
func TestNothingIsProtectedByDefault(t *testing.T) {
	protectedPaths(t, "")

	res, err := authorize(context.Background(), &sdk.FilterRequest{
		Phase: sdk.PhaseAuthorize, Path: "/api/system/users",
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if got := res.Inspect(); got.Action != sdk.ActionContinue {
		t.Errorf("action = %v, want Continue with no prefixes configured", got.Action)
	}
}

// --- authenticate: identity, through the seam ---------------------------

func withResolver(t *testing.T, fn func(context.Context, string) (Key, bool, error)) {
	t.Helper()
	prev := resolveKey
	resolveKey = fn
	t.Cleanup(func() { resolveKey = prev })
}

func authOnce(t *testing.T, header string) (sdk.FilterDecision, error) {
	t.Helper()
	res, err := authenticate(context.Background(), &sdk.FilterRequest{
		Phase:  sdk.PhaseAuthenticate,
		Method: http.MethodGet,
		Path:   "/api/plugins/notes/items",
		Header: http.Header{"Authorization": []string{header}},
	})
	if err != nil {
		return sdk.FilterDecision{}, err
	}
	return res.Inspect(), nil
}

func TestAValidKeyBecomesAnIdentity(t *testing.T) {
	withResolver(t, func(_ context.Context, hash string) (Key, bool, error) {
		if hash != hashKey("secret-value") {
			t.Errorf("looked up %q, want the hash of the presented key", hash)
		}
		return Key{UserID: "42", Name: "svc", Roles: []string{"reader", "writer"}}, true, nil
	})

	got, err := authOnce(t, "Bearer secret-value")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	id := got.Identity
	if id == nil {
		t.Fatalf("no identity was set: %+v", got)
	}
	if id.UserID != "42" || id.Username != "svc" || len(id.Roles) != 2 {
		t.Errorf("identity = %+v", id)
	}
}

// Three cases that all mean "carry on anonymously", and the reason matters:
// establishing identity is this phase's job, refusing is authorize's. A filter
// that refused here would make every public route require a key.
func TestUnusableCredentialsLeaveTheCallerAnonymous(t *testing.T) {
	tests := []struct {
		name   string
		header string
		key    Key
		found  bool
	}{
		{name: "no credential offered", header: ""},
		{name: "a key nobody issued", header: "Bearer nope"},
		{name: "a revoked key", header: "Bearer old", key: Key{UserID: "42", Revoked: true}, found: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withResolver(t, func(context.Context, string) (Key, bool, error) {
				return tc.key, tc.found, nil
			})

			got, err := authOnce(t, tc.header)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if got.Action != sdk.ActionContinue {
				t.Fatalf("action = %v, want Continue: refusing is the authorize phase's job", got.Action)
			}
			if got.Identity != nil {
				t.Errorf("an identity was established from an unusable credential: %+v", got.Identity)
			}
		})
	}
}

// The one case that must not be Continue. This filter is declared fail_closed,
// so returning the error makes Core refuse the request rather than let it
// through unauthenticated — which is the entire reason to declare it that way.
// Swallowing this error would turn a store outage into an open door.
func TestAStoreFailureIsAnErrorRatherThanAnonymousAccess(t *testing.T) {
	withResolver(t, func(context.Context, string) (Key, bool, error) {
		return Key{}, false, errors.New("the store is unreachable")
	})

	_, err := authOnce(t, "Bearer whatever")
	if err == nil {
		t.Fatal("a store failure was reported as a successful anonymous request; " +
			"with fail_closed that is the difference between refusing and admitting")
	}
}

// --- the pure helpers ---------------------------------------------------

func TestBearerKeyAcceptsBothSchemes(t *testing.T) {
	tests := []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"ApiKey abc", "abc"},
		{"Bearer  abc  ", "abc"},
		{"bearer abc", ""}, // case-sensitive by design; the RFC scheme is not
		{"Basic abc", ""},
		{"abc", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := bearerKey(tc.header); got != tc.want {
			t.Errorf("bearerKey(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestSplitListDropsEmptyEntries(t *testing.T) {
	got := splitList(" /a/ , ,/b/,  ")
	if len(got) != 2 || got[0] != "/a/" || got[1] != "/b/" {
		t.Errorf("splitList = %#v, want the two non-empty prefixes", got)
	}
	if splitList("") != nil {
		t.Error("an empty setting must produce no prefixes, not one empty prefix — " +
			"an empty prefix matches every path and would protect the whole site")
	}
}

// --- the in-process cache -----------------------------------------------

func TestTheLocalCacheExpiresAndCanBeDropped(t *testing.T) {
	hash := hashKey("local-test")
	localPut(hash, Key{UserID: "42"}, 50*time.Millisecond)

	if _, ok := localGet(hash); !ok {
		t.Fatal("the entry was not readable straight after being written")
	}

	localDrop(hash)
	if _, ok := localGet(hash); ok {
		t.Error("a dropped entry was still served; revoking a key depends on this")
	}

	// Expiry is what bounds how long this process can serve a key that was
	// withdrawn elsewhere — no other process can clear this map.
	localPut(hash, Key{UserID: "42"}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := localGet(hash); ok {
		t.Error("an expired entry was still served")
	}
	localDrop(hash)
}
