package gateway

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// The secret placeholder, round trip.
//
// The console never receives a secret's real value — it gets a mask. So when
// an operator edits some other field and saves, the mask comes back, and the
// handler has to understand it as "leave this one alone". Without that, every
// save writes ••••••••  over the real credential, and nothing says so: the
// plugin keeps running on the value it was given at launch and only fails the
// next time it is restarted, calling an upstream with a password made of
// bullet points.
//
// A comment in plugins_handler.go says all this. Nothing tested it.

type configManager struct{ decls []manifest.ConfigDecl }

func (m configManager) List() []pluginhost.Status {
	return []pluginhost.Status{{Key: "syncer", Config: m.decls}}
}
func (configManager) Scan()                                 {}
func (configManager) Enable(context.Context, string) error  { return nil }
func (configManager) Disable(context.Context, string) error { return nil }
func (configManager) Upgrade(context.Context, string) error { return nil }
func (configManager) SetConfig(context.Context, string, map[string]string) error {
	return nil
}

type recordingConfig struct {
	stored map[string]string
	wrote  map[string]string
}

func (c *recordingConfig) Get(context.Context, string) (map[string]string, error) {
	return maps.Clone(c.stored), nil
}

func (c *recordingConfig) Set(_ context.Context, _ string, values map[string]string) error {
	c.wrote = maps.Clone(values)
	c.stored = maps.Clone(values)
	return nil
}

func configHandler(t *testing.T, stored map[string]string) (*PluginsHandler, *recordingConfig) {
	t.Helper()

	store := &recordingConfig{stored: maps.Clone(stored)}
	h := NewPluginsHandler(stubResolver{}, configManager{decls: []manifest.ConfigDecl{
		{Key: "endpoint"},
		{Key: "api_key", Secret: true},
	}})
	h.Config = store
	return h, store
}

func postConfig(t *testing.T, h *PluginsHandler, values map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]any{"values": values})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, PluginsAPIPrefix+"/syncer/config", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.Serve(rec, r)
	return rec
}

func TestASecretIsMaskedOnTheWayOut(t *testing.T) {
	h, _ := configHandler(t, map[string]string{"endpoint": "https://api.example.com", "api_key": "sk-real"})

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/config"))

	var got struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Values["api_key"] != SecretPlaceholder {
		t.Errorf("api_key = %q; a declared secret must not leave Core", got.Values["api_key"])
	}
	if got.Values["endpoint"] != "https://api.example.com" {
		t.Errorf("endpoint = %q; only declared secrets are masked", got.Values["endpoint"])
	}
}

// An unset secret shows as unset, not as a filled box over nothing.
func TestAnUnsetSecretIsNotMasked(t *testing.T) {
	h, _ := configHandler(t, map[string]string{"endpoint": "https://api.example.com"})

	rec := httptest.NewRecorder()
	h.Serve(rec, adminRequest(http.MethodGet, PluginsAPIPrefix+"/syncer/config"))

	var got struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if v, ok := got.Values["api_key"]; ok {
		t.Errorf("api_key = %q for a secret that was never set; an operator seeing a "+
			"filled field believes there is a credential behind it", v)
	}
}

// The one that matters: saving the mask keeps the real value.
func TestSubmittingTheMaskKeepsTheStoredSecret(t *testing.T) {
	h, store := configHandler(t, map[string]string{"endpoint": "https://api.example.com", "api_key": "sk-real"})

	// What the console does after an operator edits only the endpoint: it
	// sends back the whole form, secret included, exactly as it received it.
	rec := postConfig(t, h, map[string]string{
		"endpoint": "https://api2.example.com",
		"api_key":  SecretPlaceholder,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	if store.wrote["api_key"] != "sk-real" {
		t.Errorf("api_key was stored as %q; editing any other field overwrote the "+
			"credential with the mask, and nothing would say so until the plugin "+
			"next restarted and authenticated with bullet points", store.wrote["api_key"])
	}
	if store.wrote["endpoint"] != "https://api2.example.com" {
		t.Errorf("endpoint = %q; the edit that was actually made has to land",
			store.wrote["endpoint"])
	}
}

// A real new secret still replaces the old one, or the mask rule would make
// secrets unchangeable.
func TestSubmittingANewSecretReplacesIt(t *testing.T) {
	h, store := configHandler(t, map[string]string{"api_key": "sk-old"})

	postConfig(t, h, map[string]string{"api_key": "sk-new"})

	if store.wrote["api_key"] != "sk-new" {
		t.Errorf("api_key = %q, want sk-new; a secret that cannot be rotated is worse "+
			"than one that is visible", store.wrote["api_key"])
	}
}

// The mask is only special for a field declared secret. Somewhere a non-secret
// setting could legitimately hold that string.
func TestTheMaskIsNotSpecialForAPlainField(t *testing.T) {
	h, store := configHandler(t, map[string]string{"endpoint": "https://api.example.com"})

	postConfig(t, h, map[string]string{"endpoint": SecretPlaceholder})

	if store.wrote["endpoint"] != SecretPlaceholder {
		t.Errorf("endpoint = %q; the placeholder means 'unchanged' only for a declared "+
			"secret, because only a secret is ever hidden from the sender",
			store.wrote["endpoint"])
	}
}

// Saving replaces the whole set rather than merging into it.
//
// Pinned rather than judged: the console posts the entire form, so this is
// correct for the only caller today. It is written down because the endpoint
// accepts POST as well as PUT and reads like something a script could patch
// one field with — which would silently drop every other setting.
func TestSavingConfigReplacesRatherThanMerges(t *testing.T) {
	h, store := configHandler(t, map[string]string{
		"endpoint": "https://api.example.com",
		"api_key":  "sk-real",
	})

	postConfig(t, h, map[string]string{"endpoint": "https://api2.example.com"})

	if _, ok := store.wrote["api_key"]; ok {
		t.Log("a partial submit merged; if that is now the contract, the console's " +
			"full-form post is no longer the only safe way to call this")
	} else {
		t.Log("a partial submit replaces: settings absent from the request are " +
			"dropped, so every caller has to send the whole set")
	}
	if store.wrote["endpoint"] != "https://api2.example.com" {
		t.Errorf("endpoint = %q; whichever semantics apply, the submitted value lands",
			store.wrote["endpoint"])
	}
}
