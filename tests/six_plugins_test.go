package tests

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every shipped example in one Core.
//
// This is the product claim stated plainly: plugins written by different teams
// intervening at different points of one request. Until now the largest
// composition tested was three, and the interesting interactions were all
// pairwise — an identity reaching one other plugin, a rate limiter governing
// one other plugin.
//
// The claim worth checking at six is transitive: an identity established by
// apikey's authenticate filter, used by notes to serve, and recorded by
// audit's log filter — three separate processes, one request, and the thing
// being passed between them never touches the client.

// sixPluginStack installs every shipped example in one Core.
//
// The list it used to hold said six when there were seven, so the composition
// this file exists to exercise quietly stopped being "all of them". Taken from
// the directory now, so adding an example puts it in the stack rather than
// leaving the claim in the comment above untrue.
func sixPluginStack(t *testing.T, config map[string]map[string]string) (string, func()) {
	t.Helper()

	url, _, _ := stackOf(t, config, shippedExamples(t)...)
	return url, func() {}
}

// seedKey provisions a key the way an operator has to provision the first one.
//
// The apikey plugin cannot bootstrap itself: minting requires an admin, and in
// a Core with session authentication that admin is a person with a session.
// This test's gateway has no session store, so the first credential is written
// where the plugin looks for it — which is exactly the out-of-band step a real
// deployment performs once.
func seedKey(t *testing.T, handle *sql.DB, plaintext, userID, name string) {
	t.Helper()

	sum := sha256.Sum256([]byte(plaintext))
	hash := hex.EncodeToString(sum[:])

	doc, err := json.Marshal(map[string]any{
		"hash": hash, "user_id": userID, "name": name,
		"roles": []string{"admin"}, "label": "seeded",
		"created": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}
	if _, err := handle.Exec(
		`INSERT INTO ext_apikey_keys (id, data, version) VALUES ($1, $2, 1)
		 ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`, hash, doc); err != nil {
		t.Fatalf("seeding the key: %v", err)
	}
}

// Every example runs at once and the system still serves.
//
// The floor: six processes, five of them with filters on the request path,
// and an ordinary request still works. A composition that merely starts is
// not interesting, but one that does not is the end of the exercise.
func TestAllSixExamplesServeTogether(t *testing.T) {
	url, done := sixPluginStack(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
		"apikey":    {"protected_prefixes": "", "cache_ttl": "60s", "local_ttl": "5s"},
		"redact":    {"fields": "email", "mask": "[gone]"},
	})
	defer done()

	client := warmClientPlain()
	for _, path := range []string{
		"/api/plugins/notes/stats",
		"/api/plugins/ratelimit/stats",
		"/api/plugins/inventory/items",
	} {
		if code := getStatus(t, client, url+path); code != http.StatusOK {
			t.Errorf("GET %s = %d with all six examples installed", path, code)
		}
	}
}

// The transitive claim: an identity one plugin establishes is recorded by
// another plugin's log filter, having passed through a third that served the
// request.
//
// apikey resolves the key in the authenticate phase; notes serves; audit reads
// sdk.User(ctx) in the log phase and writes it to its own table. Nothing
// between them shares memory, and the client never says who it is beyond
// presenting a key.
func TestIdentityFromApiKeyReachesTheAuditRecord(t *testing.T) {
	handle := requireDB(t)
	if _, err := handle.Exec(`TRUNCATE ext_audit_audit_log`); err != nil {
		t.Logf("clearing earlier audit rows: %v", err)
	}

	url, done := sixPluginStack(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
		"apikey":    {"protected_prefixes": "", "cache_ttl": "60s", "local_ttl": "5s"},
		"redact":    {"fields": "nothing", "mask": "[gone]"},
	})
	defer done()

	const actor = "svc-importer"
	const key = "seeded-key-for-the-composed-test"
	seedKey(t, handle, key, "4242", actor)

	// A write to a different plugin entirely, carrying only the key.
	req, err := http.NewRequest(http.MethodPost, url+"/api/plugins/notes/notes",
		strings.NewReader(`{"title":"composed"}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := warmClientPlain().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the write returned %d", resp.StatusCode)
	}

	// The log phase is asynchronous and the record goes through the audit
	// plugin's own table.
	var users []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		users = auditUsers(t, handle)
		for _, u := range users {
			if u == actor {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the audit table recorded %v; the identity apikey established never reached "+
		"the plugin that records it, three processes away", users)
}

// auditUsers reads the user field the audit plugin recorded. Straight to the
// table, since the plugin's own listing route is admin-only.
func auditUsers(t *testing.T, handle *sql.DB) []string {
	t.Helper()

	rows, err := handle.Query(`SELECT data->>'user' FROM ext_audit_audit_log`)
	if err != nil {
		t.Fatalf("reading audit rows: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var u sql.NullString
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, u.String)
	}
	return out
}

// The same request without the key records an anonymous actor.
//
// The other direction, and it is what makes the test above mean anything: if
// audit recorded a constant, or recorded whatever the client claimed, the
// positive test would pass for a system where no identity travelled at all.
func TestWithoutAKeyTheAuditRecordIsAnonymous(t *testing.T) {
	handle := requireDB(t)
	if _, err := handle.Exec(`TRUNCATE ext_audit_audit_log`); err != nil {
		t.Logf("clearing earlier audit rows: %v", err)
	}

	url, done := sixPluginStack(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
		"apikey":    {"protected_prefixes": "", "cache_ttl": "60s", "local_ttl": "5s"},
		"redact":    {"fields": "nothing", "mask": "[gone]"},
	})
	defer done()

	req, err := http.NewRequest(http.MethodPost, url+"/api/plugins/notes/notes",
		strings.NewReader(`{"title":"anonymous"}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A header the client made up, to check nothing downstream believes it.
	req.Header.Set("X-User-Id", "9999")

	resp, err := warmClientPlain().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var users []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if users = auditUsers(t, handle); len(users) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(users) == 0 {
		t.Fatal("nothing was audited at all")
	}
	t.Logf("audited actor without a key: %q", users[0])

	for _, u := range users {
		if u == "svc-importer" || u == "9999" {
			t.Errorf("audited actor = %q for a request carrying no credential; either a "+
				"stale identity leaked across requests or a client-supplied header was "+
				"believed", u)
		}
	}
}
