// Command apikey is an example plugin that authenticates requests.
//
// It is the one shape the other examples do not cover, and the one that
// matters most to get right: a filter that runs in the authenticate phase and
// tells Core who the caller is. Everything downstream — other plugins' filters,
// their handlers, the audit log — believes what this returns.
//
//	CGO_ENABLED=0 go build -o apikey/bin/plugin ./extension-example/apikey
//	cp extension-example/apikey/manifest.yaml apikey/
//	PLUGIN_DIR=$(pwd) go run ./core
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// Key is what is stored. The key itself is not: only its hash, so a leak of
// the collection is not a leak of every credential.
type Key struct {
	Hash    string   `json:"hash"`
	Label   string   `json:"label"`
	UserID  string   `json:"user_id"`
	Name    string   `json:"name"`
	Roles   []string `json:"roles"`
	Revoked bool     `json:"revoked"`
	Created string   `json:"created"`
}

var settings struct {
	sync.RWMutex
	cacheTTL  time.Duration
	localTTL  time.Duration
	protected []string
}

// local is this process's own cache, in front of Core's.
//
// sdk.Cache lives in Core, so a hit on it is still a cross-process round trip:
// measured at 124µs per authenticated request against 45µs for a request with
// no credential at all. An authenticate filter is paid by every request in the
// system, so 79µs of that is 79µs on traffic belonging to plugins that have
// nothing to do with this one.
//
// The cost of the local layer is revocation latency: this process keeps
// serving a withdrawn key until its own entry expires, and no other process
// can clear it. Hence the short TTL — seconds, not the minute the shared layer
// gets — and hence local_ttl being configurable separately.
var local struct {
	sync.RWMutex
	entries map[string]localEntry
}

type localEntry struct {
	key     Key
	expires time.Time
}

func localGet(hash string) (Key, bool) {
	local.RLock()
	defer local.RUnlock()
	e, ok := local.entries[hash]
	if !ok || time.Now().After(e.expires) {
		return Key{}, false
	}
	return e.key, true
}

func localPut(hash string, key Key, ttl time.Duration) {
	local.Lock()
	defer local.Unlock()
	if local.entries == nil {
		local.entries = map[string]localEntry{}
	}
	// A hard cap, because this is memory inside the plugin and the key space
	// is chosen by whoever sends the requests. Dropping everything is crude
	// but bounded, and the shared layer refills it.
	if len(local.entries) >= 4096 {
		local.entries = map[string]localEntry{}
	}
	local.entries[hash] = localEntry{key: key, expires: time.Now().Add(ttl)}
}

func localDrop(hash string) {
	local.Lock()
	defer local.Unlock()
	delete(local.entries, hash)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /keys", issueKey)
	mux.HandleFunc("DELETE /keys/{hash}", revokeKey)
	mux.HandleFunc("GET /keys", listKeys)

	log.SetPrefix("[apikey] ")

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhaseAuthenticate: authenticate,
			sdk.PhaseAuthorize:    authorize,
		},
		OnConfigChanged: configure,
	})
}

// configure applies admin settings.
//
// A named function rather than a closure written inline above, for the reason
// extension-example/redact records: a test can call this, and it cannot call an
// anonymous function passed to sdk.Serve. What it decides is worth testing —
// protected_prefixes is the list that makes authorize refuse anyone, so a typo
// here is an authorization hole rather than a formatting problem.
func configure(cfg map[string]string) {
	settings.Lock()
	defer settings.Unlock()
	if d, err := time.ParseDuration(cfg["cache_ttl"]); err == nil && d > 0 {
		settings.cacheTTL = d
	} else if settings.cacheTTL == 0 {
		settings.cacheTTL = 60 * time.Second
	}
	if d, err := time.ParseDuration(cfg["local_ttl"]); err == nil && d > 0 {
		settings.localTTL = d
	} else if settings.localTTL == 0 {
		settings.localTTL = 5 * time.Second
	}
	settings.protected = splitList(cfg["protected_prefixes"])
}

// authenticate turns an API key into an identity.
//
// Three things are worth copying from this handler, and one is worth not.
//
// Copy: it is fail-closed in the manifest, so if this plugin is down every
// request is refused rather than sailing past unauthenticated. Copy: an absent
// or unrecognised key is Continue, not Stop — this filter establishes identity
// and does not decide who may do what, which is the authorize phase's job.
// Copy: the lookup goes through the cache, because this runs on every request
// in the system and a database round trip per request is not a filter, it is
// a bottleneck.
//
// Do not copy: nothing here rate-limits key guessing. A real deployment wants
// that in front, and the ratelimit example is the shape of it.
func authenticate(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	raw := bearerKey(req.Header.Get("Authorization"))
	if raw == "" {
		// No credential offered. Not an error, and deliberately not a refusal:
		// plenty of routes are public, and this filter does not know which.
		return sdk.Continue(), nil
	}

	key, found, err := resolveKey(ctx, hashKey(raw))
	if err != nil {
		// Returning the error matters. This filter is declared fail_closed, so
		// Core refuses the request rather than letting it through
		// unauthenticated — which is the entire reason to declare it that way.
		return nil, err
	}
	if !found || key.Revoked {
		// A key that is wrong or withdrawn leaves the caller anonymous rather
		// than rejected here, for the same reason as above.
		return sdk.Continue(), nil
	}

	return sdk.Mutate().SetIdentity(&sdk.UserContext{
		UserID:   key.UserID,
		Username: key.Name,
		Roles:    key.Roles,
	}), nil
}

// authorize refuses callers who have no identity on protected paths.
//
// A separate phase from authenticate on purpose. Core only honours
// SetIdentity during authenticate and authorize, and only from a plugin
// holding filter:authenticate — so the decision about *who you are* and the
// decision about *what you may do* are separable, and a plugin can be trusted
// with the second without being trusted with the first.
func authorize(_ context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	if !isProtected(req.Path) {
		return sdk.Continue(), nil
	}
	// Nil-safe: Identity is nil for an anonymous request, and a panic in a
	// filter kills the plugin process rather than failing the request.
	if req.Identity.Authenticated() {
		return sdk.Continue(), nil
	}

	return sdk.Stop(http.StatusUnauthorized,
		[]byte(`{"error":"an API key is required for this path"}`)).
		WithHeader("WWW-Authenticate", `Bearer realm="moduless"`), nil
}

// resolveKey is the seam for the one host-backed step in the authenticate path.
//
// A package-level func var rather than the interface field extension-example/
// digest uses, because this plugin is already built around package-level state
// (the local cache is a package var, and the filters are plain functions Core
// calls by name). Fitting a struct around it to hold one dependency would be a
// larger change than the thing it buys.
//
// The trade is real and worth stating: a var is global mutable state, so a test
// that swaps it has to put it back, and two tests cannot swap it concurrently.
// digest's field carries neither problem. Prefer that shape in new plugins;
// this one shows the lighter option for code already written this way.
var resolveKey = lookup

// lookup resolves a key hash, through the cache.
//
// The cache is what makes an authenticate filter affordable: it runs on every
// request in the system, so the cost of this function is added to the cost of
// every request, including requests belonging to other plugins entirely.
func lookup(ctx context.Context, hash string) (Key, bool, error) {
	// In-process first. This is the only layer that costs nothing.
	if key, ok := localGet(hash); ok {
		return key, true, nil
	}

	settings.RLock()
	ttl, localTTL := settings.cacheTTL, settings.localTTL
	settings.RUnlock()

	var key Key
	found, err := sdk.Cache.Get(ctx, "k:"+hash, &key)
	if err == nil && found {
		localPut(hash, key, localTTL)
		return key, true, nil
	}

	found, _, err = sdk.DB.Get(ctx, "keys", hash, &key)
	if err != nil {
		return Key{}, false, err
	}
	if !found {
		return Key{}, false, nil
	}

	// A revoked key is cached too, and for the same TTL. Caching only the
	// valid ones would mean every request bearing a revoked key hits the
	// database — which is exactly the traffic an attacker generates.
	_ = sdk.Cache.Set(ctx, "k:"+hash, key, ttl)
	localPut(hash, key, localTTL)

	return key, true, nil
}

// issueKey mints a key. The plaintext is returned once and never stored.
func issueKey(w http.ResponseWriter, r *http.Request) {
	// The menu's `roles: [admin]` decides who sees the menu item, not who may
	// call this. Anyone who knows the URL can reach it, so the check is here.
	if !sdk.User(r.Context()).HasRole("admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Label  string   `json:"label"`
		UserID string   `json:"user_id"`
		Name   string   `json:"name"`
		Roles  []string `json:"roles"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.UserID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	plaintext := sdk.NewID() + sdk.NewID()
	key := Key{
		Hash:    hashKey(plaintext),
		Label:   req.Label,
		UserID:  req.UserID,
		Name:    req.Name,
		Roles:   req.Roles,
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := sdk.DB.Put(r.Context(), "keys", key.Hash, key); err != nil {
		http.Error(w, "could not store the key", http.StatusInternalServerError)
		return
	}

	// Shown once. Storing the plaintext would make this collection worth
	// stealing; storing only the hash makes it worth nothing on its own.
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":  plaintext,
		"hash": key.Hash,
		"note": "this is the only time the key is shown",
	})
}

// revokeKey withdraws a key.
func revokeKey(w http.ResponseWriter, r *http.Request) {
	if !sdk.User(r.Context()).HasRole("admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	hash := r.PathValue("hash")

	var key Key
	found, version, err := sdk.DB.Get(r.Context(), "keys", hash, &key)
	if err != nil {
		http.Error(w, "could not read the key", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such key", http.StatusNotFound)
		return
	}

	key.Revoked = true
	if _, err := sdk.DB.PutIfVersion(r.Context(), "keys", hash, key, version); err != nil {
		http.Error(w, "could not revoke the key", http.StatusConflict)
		return
	}

	// The cache is the reason a revocation is not instant. Dropping the entry
	// here makes it instant on this replica; other replicas keep serving the
	// old answer until their own entry expires, which is what cache_ttl is
	// really configuring — the worst-case window during which a withdrawn key
	// still works.
	_ = sdk.Cache.Delete(r.Context(), "k:"+hash)
	localDrop(hash)

	writeJSON(w, http.StatusOK, map[string]any{
		"revoked":      hash,
		"fully_effect": "after cache_ttl on other replicas",
	})
}

func listKeys(w http.ResponseWriter, r *http.Request) {
	if !sdk.User(r.Context()).HasRole("admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var keys []Key
	if _, err := sdk.DB.Where("keys").Sort("created").Limit(100).All(r.Context(), &keys); err != nil {
		http.Error(w, "could not list keys", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// hashKey is what is stored and what the cache is keyed on. SHA-256 rather
// than bcrypt deliberately: this runs on every request, and a deliberately
// slow hash in an authenticate filter is a denial of service anyone can
// trigger by sending a wrong key. The trade is acceptable because a key is
// 256 bits of randomness rather than a password — there is no dictionary to
// run against it.
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func bearerKey(header string) string {
	for _, prefix := range []string{"Bearer ", "ApiKey "} {
		if rest, ok := strings.CutPrefix(header, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func isProtected(path string) bool {
	settings.RLock()
	defer settings.RUnlock()
	for _, p := range settings.protected {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
