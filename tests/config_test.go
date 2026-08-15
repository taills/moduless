package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// Plugin configuration, end to end.
//
// Every piece of this feature existed and none of them were connected: a
// manifest could declare settings, Core merged the declared defaults, and
// Manager.SetConfig could push a change to running replicas — but Core was
// started with an empty in-memory store, nothing set ConfigSource, and no HTTP
// endpoint could write a value. An operator had no way to configure anything,
// and only the manifest's hard-coded defaults ever took effect.
//
// That shape — a complete contract on both ends with nothing in the middle —
// is the third one found in this codebase, after configuration push itself and
// the cron scheduler. What each had in common is that both halves had tests
// and the join had none. So these tests are deliberately about the join: what
// an operator does, and what a plugin subsequently sees.

// fakeManager is a PluginManager that records what was pushed to it.
type fakeManager struct {
	status []pluginhost.Status
	pushed map[string]map[string]string
	err    error
}

func (m *fakeManager) Scan()                                 {}
func (m *fakeManager) List() []pluginhost.Status             { return m.status }
func (m *fakeManager) Enable(context.Context, string) error  { return nil }
func (m *fakeManager) Disable(context.Context, string) error { return nil }
func (m *fakeManager) Upgrade(context.Context, string) error { return nil }
func (m *fakeManager) SetConfig(_ context.Context, key string, cfg map[string]string) error {
	if m.pushed == nil {
		m.pushed = map[string]map[string]string{}
	}
	m.pushed[key] = cfg
	return m.err
}

// memConfig is a ConfigStore with no database behind it, for the handler tests.
type memConfig struct {
	values map[string]map[string]string
	getErr error
}

func (c *memConfig) Get(_ context.Context, key string) (map[string]string, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	out := map[string]string{}
	for k, v := range c.values[key] {
		out[k] = v
	}
	return out, nil
}

func (c *memConfig) Set(_ context.Context, key string, values map[string]string) error {
	if c.values == nil {
		c.values = map[string]map[string]string{}
	}
	copied := map[string]string{}
	for k, v := range values {
		copied[k] = v
	}
	c.values[key] = copied
	return nil
}

// configFixture wires the admin endpoint over a fake manager and store.
func configFixture(t *testing.T, decls []manifest.ConfigDecl) (*gateway.PluginsHandler, *fakeManager, *memConfig) {
	t.Helper()

	mgr := &fakeManager{status: []pluginhost.Status{{Key: "widget", Config: decls}}}
	store := &memConfig{}
	// Auth left nil: these tests are about configuration, and the admin-only
	// check is exercised where it belongs, in the gateway's own tests.
	h := gateway.NewPluginsHandler(nil, mgr)
	h.Config = store
	return h, mgr, store
}

func configRequest(t *testing.T, h *gateway.PluginsHandler, method, key string, body any) (int, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request: %v", err)
		}
	}
	req := httptest.NewRequest(method, gateway.PluginsAPIPrefix+"/"+key+"/config", &buf)
	rec := httptest.NewRecorder()
	h.Serve(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

var widgetDecls = []manifest.ConfigDecl{
	{Key: "greeting", Type: "string", Default: "hello"},
	{Key: "retries", Type: "int", Default: "3"},
	{Key: "api_token", Type: "string", Secret: true},
}

// The thing that was missing: a value an operator sets is stored, and is
// pushed to the plugin that is running now.
func TestConfigSaveStoresAndPushes(t *testing.T) {
	h, mgr, store := configFixture(t, widgetDecls)

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"greeting": "hi", "retries": "5"}})
	if code != http.StatusOK {
		t.Fatalf("save returned %d: %v", code, body)
	}
	if w, ok := body["warning"]; ok {
		t.Errorf("save reported a warning: %v", w)
	}

	stored, _ := store.Get(context.Background(), "widget")
	if stored["greeting"] != "hi" || stored["retries"] != "5" {
		t.Errorf("stored = %v; the value did not reach the store, so it would be lost at the next restart", stored)
	}

	pushed := mgr.pushed["widget"]
	if pushed == nil {
		t.Fatal("nothing was pushed; the running plugin would keep the old value until it restarted")
	}
	if pushed["greeting"] != "hi" {
		t.Errorf("pushed = %v; want greeting=hi", pushed)
	}
}

// Reading back returns both halves: what the plugin declares, and what is set.
// A console with only the values cannot render a form, and one with only the
// declarations cannot show what is in effect.
func TestConfigReadReturnsDeclarationsAndValues(t *testing.T) {
	h, _, store := configFixture(t, widgetDecls)
	_ = store.Set(context.Background(), "widget", map[string]string{"greeting": "hi"})

	code, body := configRequest(t, h, http.MethodGet, "widget", nil)
	if code != http.StatusOK {
		t.Fatalf("read returned %d: %v", code, body)
	}

	declared, ok := body["declared"].([]any)
	if !ok || len(declared) != len(widgetDecls) {
		t.Fatalf("declared = %v; want the %d settings the manifest declares", body["declared"], len(widgetDecls))
	}
	values, _ := body["values"].(map[string]any)
	if values["greeting"] != "hi" {
		t.Errorf("values = %v; want the stored greeting", values)
	}
}

// A key the plugin never declared is refused.
//
// This is what makes the declaration worth writing: without it a typo sits in
// the database looking configured while the plugin goes on using its default,
// and the operator's only evidence is that nothing happened.
func TestConfigRejectsUndeclaredKey(t *testing.T) {
	h, mgr, store := configFixture(t, widgetDecls)

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"greetings": "hi"}})
	if code != http.StatusBadRequest {
		t.Fatalf("a misspelled key was accepted with %d: %v", code, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("the refusal carried no explanation")
	} else {
		t.Logf("refusal: %s", msg)
	}

	// And nothing was written or pushed on the way to refusing.
	if stored, _ := store.Get(context.Background(), "widget"); len(stored) != 0 {
		t.Errorf("the rejected write still stored %v", stored)
	}
	if mgr.pushed["widget"] != nil {
		t.Error("the rejected write was still pushed to the plugin")
	}
}

// The other direction: a declared key is accepted. A validator that refuses
// everything passes the test above.
func TestConfigAcceptsDeclaredKey(t *testing.T) {
	h, _, _ := configFixture(t, widgetDecls)

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"greeting": "hi"}})
	if code != http.StatusOK {
		t.Fatalf("a declared key was refused with %d: %v", code, body)
	}
}

// Secrets are masked on the way out, and the mask coming back in means "leave
// it alone" rather than "set it to those bullets".
//
// Without the second half, an operator changing an unrelated field saves the
// mask over the real credential, and finds out when the plugin next tries to
// use it — by which time the original is gone.
func TestConfigSecretRoundTripDoesNotDestroyTheValue(t *testing.T) {
	h, _, store := configFixture(t, widgetDecls)
	_ = store.Set(context.Background(), "widget", map[string]string{"api_token": "s3cret", "greeting": "hi"})

	_, body := configRequest(t, h, http.MethodGet, "widget", nil)
	values, _ := body["values"].(map[string]any)
	masked, _ := values["api_token"].(string)
	if masked == "s3cret" {
		t.Fatal("the secret was returned in clear text")
	}
	if masked == "" {
		t.Fatal("a secret that is set came back empty; the console would show an unconfigured field")
	}

	// Submit exactly what was read back, which is what a console form does.
	code, saveBody := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"api_token": masked, "greeting": "hello"}})
	if code != http.StatusOK {
		t.Fatalf("save returned %d: %v", code, saveBody)
	}

	stored, _ := store.Get(context.Background(), "widget")
	if stored["api_token"] != "s3cret" {
		t.Errorf("api_token = %q after saving an unrelated field; the mask overwrote the real value", stored["api_token"])
	}
	if stored["greeting"] != "hello" {
		t.Errorf("greeting = %q; the edit that was actually made did not apply", stored["greeting"])
	}
}

// A secret can still be changed — masking must not make it read-only.
func TestConfigSecretCanBeReplaced(t *testing.T) {
	h, _, store := configFixture(t, widgetDecls)
	_ = store.Set(context.Background(), "widget", map[string]string{"api_token": "old"})

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"api_token": "new"}})
	if code != http.StatusOK {
		t.Fatalf("save returned %d: %v", code, body)
	}
	stored, _ := store.Get(context.Background(), "widget")
	if stored["api_token"] != "new" {
		t.Errorf("api_token = %q; a secret that was deliberately changed did not change", stored["api_token"])
	}
}

// A push that fails must not look like a save that failed. The value is
// stored, so the remedy is to look at the plugin, not to press save again.
func TestConfigReportsPushFailureSeparatelyFromSaveFailure(t *testing.T) {
	h, mgr, store := configFixture(t, widgetDecls)
	mgr.err = fmt.Errorf("plugin is not running")

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"greeting": "hi"}})
	if code != http.StatusOK {
		t.Fatalf("a stored-but-undelivered value was reported as a failed save (%d): %v", code, body)
	}
	warning, _ := body["warning"].(string)
	if warning == "" {
		t.Fatal("the failure to deliver was not reported at all")
	}
	t.Logf("warning: %s", warning)

	if stored, _ := store.Get(context.Background(), "widget"); stored["greeting"] != "hi" {
		t.Error("the value was not stored, so restarting the plugin would not fix it either")
	}
}

// Core without a database says so, rather than accepting a value it cannot
// keep. Silently losing it at the next restart is the worse failure: it works
// once, which is exactly long enough to be trusted.
func TestConfigWithoutDatabaseRefusesClearly(t *testing.T) {
	mgr := &fakeManager{status: []pluginhost.Status{{Key: "widget", Config: widgetDecls}}}
	h := gateway.NewPluginsHandler(nil, mgr) // no store

	code, body := configRequest(t, h, http.MethodPost, "widget",
		map[string]any{"values": map[string]string{"greeting": "hi"}})
	if code == http.StatusOK {
		t.Fatal("a value was accepted with nowhere to store it")
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatal("the refusal carried no explanation")
	}
	t.Logf("refusal: %s", msg)
	if !bytes.Contains([]byte(msg), []byte("DATABASE_URL")) {
		t.Errorf("the refusal does not say what to do about it: %q", msg)
	}
}

// An unknown plugin is a 404 rather than an empty form.
func TestConfigUnknownPlugin(t *testing.T) {
	h, _, _ := configFixture(t, widgetDecls)

	code, _ := configRequest(t, h, http.MethodGet, "nosuch", nil)
	if code != http.StatusNotFound {
		t.Errorf("status = %d for a plugin that does not exist; want 404", code)
	}
}

// --- the database half -------------------------------------------------------

func configDB(t *testing.T) *hostsvc.DBConfig {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	conn, err := db.InitDB(url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Migrations run against this database elsewhere; if the table is not
	// there, that is what this test is here to notice.
	if _, err := conn.Exec(`DELETE FROM plugin_config WHERE plugin_key LIKE 'cfgtest%'`); err != nil {
		t.Fatalf("the plugin_config table is not usable: %v", err)
	}
	return hostsvc.NewDBConfig(conn)
}

// The point of the database-backed store: what is written is still there when
// it is read by a different process.
func TestDBConfigPersists(t *testing.T) {
	store := configDB(t)
	ctx := context.Background()

	if err := store.Set(ctx, "cfgtest", map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get(ctx, "cfgtest")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["a"] != "1" || got["b"] != "2" {
		t.Errorf("got %v; want a=1 b=2", got)
	}
}

// Saving is a replace, not a merge: a setting the operator cleared has to
// actually go away rather than linger from the previous save.
func TestDBConfigSaveIsReplaceNotMerge(t *testing.T) {
	store := configDB(t)
	ctx := context.Background()

	if err := store.Set(ctx, "cfgtest", map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(ctx, "cfgtest", map[string]string{"a": "9"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := store.Get(ctx, "cfgtest")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, still := got["b"]; still {
		t.Errorf("got %v; b was removed but is still stored", got)
	}
	if got["a"] != "9" {
		t.Errorf("got %v; want a=9", got)
	}
}

// One plugin's settings are not another's. Config keys are short words that
// different plugins will reuse — every plugin will have a "timeout".
func TestDBConfigIsPerPlugin(t *testing.T) {
	store := configDB(t)
	ctx := context.Background()

	if err := store.Set(ctx, "cfgtest-a", map[string]string{"timeout": "1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(ctx, "cfgtest-b", map[string]string{"timeout": "2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	a, _ := store.Get(ctx, "cfgtest-a")
	b, _ := store.Get(ctx, "cfgtest-b")
	if a["timeout"] != "1" || b["timeout"] != "2" {
		t.Errorf("a=%v b=%v; one plugin's settings reached the other", a, b)
	}
}

// A plugin with nothing stored reads as empty rather than as an error — that
// is every plugin's first launch.
func TestDBConfigEmptyIsNotAnError(t *testing.T) {
	store := configDB(t)

	got, err := store.Get(context.Background(), "cfgtest-never-set")
	if err != nil {
		t.Fatalf("reading an unconfigured plugin failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want nothing", got)
	}
}
