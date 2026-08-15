package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// The apikey example, which is the one that decides who the caller is.
//
// Every other filter in this repository observes or refuses. This one tells
// Core an identity, and everything downstream — other plugins' filters, their
// handlers, the audit log — believes it. So the questions worth asking are not
// "does it work" but "what stops it working when it should not": does Core
// actually enforce the filter:authenticate permission, and does it enforce the
// phase, or are those two claims that only exist in a comment?

// authPlugin launches the apikey example with a chosen permission set, wired
// to a real document store and cache.
func authPlugin(t *testing.T, root string, granted []string) (*pluginhost.Instance, *pluginhost.Package) {
	t.Helper()

	installExampleAs(t, root, "apikey", "apikey", "../extension-example/apikey")
	pkg, err := pluginhost.LoadPackage(root + "/apikey")
	if err != nil {
		t.Fatalf("loading the apikey example: %v", err)
	}

	handle := requireDB(t)
	data := hostsvc.NewCMDSData(handle, db.NewCMDSManager(handle), db.NewTxRegistry())
	if err := data.ProvisionSchema("apikey", []db.CollectionSchema{{Name: "keys"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := handle.Exec(`DELETE FROM ext_apikey_keys`); err != nil {
		t.Fatalf("clearing keys: %v", err)
	}

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "apikey",
		InstanceID: "apikey-0",
		Version:    "1.0.0",
		BinaryPath: root + "/apikey/bin/plugin",
		Checksum:   checksum(t, root+"/apikey/bin/plugin"),
		HostImpl: hostsvc.New("apikey", granted, hostsvc.Deps{
			Data:  data,
			Cache: hostsvc.NewMemoryCache(100),
			Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
				return map[string]string{
					"cache_ttl":          "60s",
					"local_ttl":          "5s",
					"protected_prefixes": "/api/plugins/notes/",
				}, nil
			}),
		}),
		GrantedPermissions: granted,
		Env:                []string{"PATH=/usr/bin:/bin"},
		DevMode:            true,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(inst.Kill)
	return inst, pkg
}

// mintKey asks the plugin for a key, as an admin would.
func mintKey(t *testing.T, inst *pluginhost.Instance, userID, name string, roles []string) string {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"user_id": userID, "name": name, "roles": roles, "label": "test",
	})
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodPost,
		Path:   "/keys",
		Body:   body,
		// An admin session, which Core resolves and passes to the plugin.
		Identity: &pb.Identity{UserId: "1", Username: "admin", Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatalf("minting a key: %v", err)
	}
	if resp.GetStatusCode() != http.StatusCreated {
		t.Fatalf("minting returned %d: %s", resp.GetStatusCode(), resp.GetBody())
	}

	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(resp.GetBody(), &out); err != nil {
		t.Fatalf("reply is not JSON: %s", resp.GetBody())
	}
	if out.Key == "" {
		t.Fatalf("no key in the reply: %s", resp.GetBody())
	}
	return out.Key
}

// filterOnce runs the plugin's filter for one phase and returns the verdict.
func filterOnce(t *testing.T, inst *pluginhost.Instance, phase pb.Phase, path, authHeader string,
	identity *pb.Identity) *pb.FilterResponse {
	t.Helper()

	headers := map[string]*pb.HeaderValues{}
	if authHeader != "" {
		headers["Authorization"] = &pb.HeaderValues{Values: []string{authHeader}}
	}
	resp, err := inst.Client.Filter(context.Background(), &pb.FilterRequest{
		Phase:    phase,
		Method:   http.MethodGet,
		Path:     path,
		Headers:  headers,
		Identity: identity,
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	return resp
}

// A valid key becomes an identity. This is the example doing its job.
func TestApiKeyGrantsIdentity(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})

	key := mintKey(t, inst, "42", "svc-reporting", []string{"reader"})

	resp := filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/api/plugins/notes/items",
		"Bearer "+key, nil)

	if resp.GetAction() != pb.FilterResponse_ACTION_MUTATE {
		t.Fatalf("action = %v, want MUTATE", resp.GetAction())
	}
	id := resp.GetMutation().GetSetIdentity()
	if id == nil {
		t.Fatal("no identity was set")
	}
	if id.GetUserId() != "42" || id.GetUsername() != "svc-reporting" {
		t.Errorf("identity = %+v", id)
	}
	if len(id.GetRoles()) != 1 || id.GetRoles()[0] != "reader" {
		t.Errorf("roles = %v", id.GetRoles())
	}
}

// A revoked key stops working, and the caller is left anonymous rather than
// rejected — refusing is the authorize phase's job.
func TestRevokedKeyGrantsNothing(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc", []string{"reader"})

	// Establish that it worked first, or the revocation proves nothing.
	if got := filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil); got.GetMutation().GetSetIdentity() == nil {
		t.Fatal("the key did not work before it was revoked")
	}

	// Find its hash the way an admin would, then revoke.
	list, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/keys",
		Identity: &pb.Identity{UserId: "1", Username: "admin", Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatalf("listing keys: %v", err)
	}
	var listing struct {
		Keys []struct {
			Hash string `json:"hash"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(list.GetBody(), &listing); err != nil || len(listing.Keys) != 1 {
		t.Fatalf("unexpected listing: %s", list.GetBody())
	}

	del, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodDelete, Path: "/keys/" + listing.Keys[0].Hash,
		Identity: &pb.Identity{UserId: "1", Username: "admin", Roles: []string{"admin"}},
	})
	if err != nil || del.GetStatusCode() != http.StatusOK {
		t.Fatalf("revoking: %v %d %s", err, del.GetStatusCode(), del.GetBody())
	}

	got := filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil)
	if got.GetMutation().GetSetIdentity() != nil {
		t.Error("a revoked key still granted an identity")
	}
	if got.GetAction() == pb.FilterResponse_ACTION_SHORT_CIRCUIT {
		t.Error("the authenticate phase refused the request; that is the authorize phase's job")
	}
}

// Minting a key needs an admin. The menu's roles list decides who sees the
// menu item, not who may call the route — a distinction that has already
// shipped an all-readable audit log once in this repository.
func TestMintingAKeyNeedsAdmin(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})

	body, _ := json.Marshal(map[string]any{"user_id": "9", "name": "sneaky"})
	for _, tc := range []struct {
		name     string
		identity *pb.Identity
	}{
		{"anonymous", nil},
		{"an ordinary user", &pb.Identity{UserId: "7", Username: "bob", Roles: []string{"user"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
				Method: http.MethodPost, Path: "/keys", Body: body, Identity: tc.identity,
			})
			if err != nil {
				t.Fatalf("HandleHTTP: %v", err)
			}
			if resp.GetStatusCode() != http.StatusForbidden {
				t.Errorf("status = %d, want 403; anyone who knows the URL can reach this route",
					resp.GetStatusCode())
			}
		})
	}
}

// --- what stops it -----------------------------------------------------------

// Core discards a set_identity from a plugin that does not hold
// filter:authenticate.
//
// This is the check the whole permission exists for, and it lives on Core's
// side of the connection: the plugin still returns the mutation and Core
// throws it away. Driven through the real pipeline rather than by calling the
// applying function, because "the plugin cannot decline to be checked" is a
// claim about where the check sits, and only the real path can support it.
func identityAfterFilter(t *testing.T, inst *pluginhost.Instance, allow bool, key string) *pipeline.RequestContext {
	t.Helper()

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "apikey",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "apikey", manifest.FilterDecl{
			Name:  "authenticate",
			Phase: manifest.PhaseAuthenticate,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
		AllowIdentityMutation: allow,
	})

	snap := reg.Current()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rc := pipeline.NewRequestContext("trace", req, "127.0.0.1")
	defer rc.ReleaseAdmissions()

	var r pipeline.Runner
	r.OnFilterError = func(f *pipeline.Filter, err error) { t.Logf("filter %s: %v", f.Label(), err) }
	r.Run(context.Background(), snap.Chain(), snap, pb.Phase_PHASE_AUTHENTICATE, rc)
	return rc
}

func TestIdentityMutationNeedsThePermission(t *testing.T) {
	// db and cache, but not filter:authenticate.
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache"})
	key := mintKey(t, inst, "42", "svc", []string{"admin"})

	// The plugin does attempt it — otherwise this test proves nothing.
	attempt := filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil)
	if attempt.GetMutation().GetSetIdentity() == nil {
		t.Fatal("the plugin did not attempt a mutation; nothing to enforce against")
	}

	rc := identityAfterFilter(t, inst, false, key)
	if rc.Identity != nil {
		t.Errorf("identity = %+v; a plugin without filter:authenticate set the caller",
			rc.Identity)
	}
}

// The same mutation from the same plugin, with the permission granted, is
// honoured. Without this the test above passes for a Core that ignores every
// identity mutation — a different bug with the same symptom.
func TestIdentityMutationIsHonouredWithThePermission(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc", []string{"reader"})

	rc := identityAfterFilter(t, inst, true, key)
	if rc.Identity == nil || rc.Identity.GetUserId() != "42" {
		t.Errorf("identity = %+v, want user 42", rc.Identity)
	}
}

// The manifest declares what it needs, and Core reads it. A plugin asking for
// filter:authenticate is asking to decide who the caller is, and that has to
// be visible to whoever approves it rather than buried in the code.
func TestApiKeyManifestDeclaresWhatItDoes(t *testing.T) {
	root := t.TempDir()
	installExampleAs(t, root, "apikey", "apikey", "../extension-example/apikey")

	pkg, err := pluginhost.LoadPackage(root + "/apikey")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	var hasAuth bool
	for _, p := range pkg.Manifest.Permissions {
		if p == "filter:authenticate" {
			hasAuth = true
		}
	}
	if !hasAuth {
		t.Error("the example sets identity but does not declare filter:authenticate; " +
			"Core would discard its mutations")
	}

	// Both security filters must be fail-closed. An authenticator that fails
	// open lets every request through unauthenticated the moment the plugin
	// has a bad minute, which is the opposite of what it is for.
	for _, f := range pkg.Manifest.Filters {
		if f.Phase == manifest.PhaseAuthenticate || f.Phase == manifest.PhaseAuthorize {
			if !f.FailClosed {
				t.Errorf("filter %q in the %s phase is fail-open; a broken authenticator "+
					"would wave every request through", f.Name, f.Phase)
			}
			if f.TimeoutMS <= 0 || f.TimeoutMS > 200 {
				t.Errorf("filter %q has timeout_ms=%d; this runs on every request in the "+
					"system", f.Name, f.TimeoutMS)
			}
		}
	}
}

// The cache is what makes an authenticate filter affordable, and this checks
// it is actually being used: the second lookup of the same key must not reach
// the database.
func TestKeyLookupIsCached(t *testing.T) {
	root := t.TempDir()
	inst, _ := authPlugin(t, root, []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc", []string{"reader"})

	// Warm it.
	filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil)

	// Now delete the row underneath. A cached answer still resolves; an
	// uncached one cannot.
	handle := requireDB(t)
	if _, err := handle.Exec(`DELETE FROM ext_apikey_keys`); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	got := filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil)
	if got.GetMutation().GetSetIdentity() == nil {
		t.Error("the second lookup went to the database; an authenticate filter runs on " +
			"every request in the system and cannot afford that")
	}
}

// Timing: what an authenticate filter costs, since it is added to every
// request whether or not it belongs to this plugin.
func TestAuthenticateFilterCost(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc", []string{"reader"})

	filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil) // warm

	const n = 200
	start := time.Now()
	for range n {
		filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "Bearer "+key, nil)
	}
	perCall := time.Since(start) / n

	// No credential at all: the cheap path, which most traffic takes.
	start = time.Now()
	for range n {
		filterOnce(t, inst, pb.Phase_PHASE_AUTHENTICATE, "/x", "", nil)
	}
	perAnon := time.Since(start) / n

	t.Logf("with a cached key: %s per call; with no credential: %s", perCall, perAnon)

	// The interesting comparison is against the no-credential path, not
	// against a wall-clock constant: it cancels out whatever the machine is
	// doing and isolates what the lookup itself costs.
	//
	// The example caches in-process, in front of sdk.Cache. That matters more
	// than it looks: sdk.Cache lives in Core, so a hit on it is still a
	// cross-process round trip. Measured before the local layer existed, a
	// cached lookup cost 124µs against 45µs for a request with no credential
	// — nearly 3x, on every request in the system including ones belonging to
	// plugins that have nothing to do with this one. With it, 43µs against
	// 39µs.
	if perAnon > 0 && perCall > perAnon*2 {
		t.Errorf("a cached lookup costs %s against %s for no credential at all — more than "+
			"double. An authenticate filter is paid by every request in the system, so this "+
			"is not the plugin's own latency, it is everyone's", perCall, perAnon)
	}
}

// --- the gateway ---------------------------------------------------------------

// stubResolver resolves exactly one session token and nothing else, which is
// what a Core with authentication enabled looks like.
type stubResolver struct{ token string }

func (s stubResolver) Resolve(tok string) (auth.User, bool) {
	if tok != "" && tok == s.token {
		return auth.User{ID: 1, Username: "admin", Role: "admin"}, true
	}
	return auth.User{}, false
}

// authGateway serves a backend plugin behind an authenticate filter belonging
// to a different plugin — the arrangement the whole model exists for.
func authGateway(t *testing.T, authInst *pluginhost.Instance) (string, *pluginhost.Registry) {
	t.Helper()

	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "apikey",
		Instances: []*pluginhost.Instance{authInst},
		Filters: compileFilters(t, "apikey", manifest.FilterDecl{
			Name:  "authenticate",
			Phase: manifest.PhaseAuthenticate,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
		AllowIdentityMutation: true,
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	h := &gateway.PluginHandler{
		Registry: reg,
		Runner:   &pipeline.Runner{},
		// Core has authentication enabled, which is the normal deployment.
		Auth: stubResolver{token: "good-session"},
	}
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })))
	t.Cleanup(srv.Close)
	return srv.URL, reg
}

// A caller with an API key and no session reaches the plugin.
//
// This is the whole point of an authenticate-phase filter and it did not work:
// Core resolved the session, found none, and answered 401 before the
// authenticate phase ran. Every non-session scheme — API keys, JWTs, mTLS,
// signed requests — was unreachable, in a framework whose fourth requirement
// is that plugins intervene in each phase of the request lifecycle.
func TestApiKeyAuthenticatesAtTheGateway(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc", []string{"reader"})

	url, _ := authGateway(t, inst)

	req, _ := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("Core refused a request the authenticate phase would have authenticated; " +
			"the phase never ran")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// The other direction, which is the protection this must not remove: a caller
// with neither a session nor a key still gets nothing.
func TestNoCredentialStillGetsNothing(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	url, _ := authGateway(t, inst)

	resp, err := http.Get(url + "/api/plugins/hello/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; an anonymous caller reached a plugin", resp.StatusCode)
	}
}

// A wrong key is the same as no key. Worth its own case because "the filter
// ran" and "the filter approved" are different things, and a fix that runs the
// phase but ignores its verdict would pass the test above.
func TestAWrongKeyGetsNothing(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	url, _ := authGateway(t, inst)

	req, _ := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// And a session still works, unchanged. Whatever the fix is, it must not be
// "stop checking".
func TestSessionStillAuthenticates(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	url, _ := authGateway(t, inst)

	req, _ := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	req.Header.Set("Authorization", "Bearer good-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a valid session", resp.StatusCode)
	}
}

// The identity one plugin establishes reaches another plugin's handler.
//
// This is the product claim — plugins from different teams intervening at
// different phases of one request — and it had no test. Everything before this
// checked one plugin at a time: that the authenticate filter returns a
// mutation, that Core applies it. Nobody had checked that the plugin actually
// serving the request is told who the caller is.
func TestIdentityFromOnePluginReachesAnother(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	key := mintKey(t, inst, "42", "svc-reporting", []string{"reader", "writer"})

	url, _ := authGateway(t, inst)

	req, _ := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Caller"); got != "svc-reporting" {
		t.Errorf("the backend plugin saw caller %q; the identity the authenticate filter "+
			"established did not reach it", got)
	}
	if got := resp.Header.Get("X-Caller-Roles"); got != "reader,writer" {
		t.Errorf("roles = %q; want the ones the key carries", got)
	}
}

// A session identity reaches the backend too, and is not overwritten by an
// authenticate filter that had nothing to add. A filter returning Continue
// must leave what Core resolved alone.
func TestSessionIdentityReachesTheBackendUnchanged(t *testing.T) {
	inst, _ := authPlugin(t, t.TempDir(), []string{"db", "cache", "filter:authenticate"})
	url, _ := authGateway(t, inst)

	req, _ := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	req.Header.Set("Authorization", "Bearer good-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Caller"); got != "admin" {
		t.Errorf("the backend saw caller %q; Core's own session identity was lost or "+
			"overwritten by a filter that returned Continue", got)
	}
}
