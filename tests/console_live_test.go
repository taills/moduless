package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pluginhost"
)

// The headline claim, as one flow.
//
// "A plugin's menus and pages appear when it is enabled and disappear when it
// is disabled, with no page reload" is requirement four, and every piece of it
// had a test: the registry fires OnChange with a new snapshot, UIEvents
// delivers to an open subscriber, and the menu tree follows the plugin. What
// had no test is the join — and the join is one line in main.go, wiring
// OnChange to Publish. That is the shape this codebase has produced a bug in
// four times: both ends complete, nothing checking the middle.
//
// So this drives it the way an operator does. A console is open on the event
// stream. Someone clicks 停用. The console must be told, and what it fetches
// afterwards must have changed.

// consoleStack is Core's console surface, wired the way main.go wires it.
func consoleStack(t *testing.T) (url string, mgr *pluginhost.Manager, events *gateway.UIEvents) {
	t.Helper()

	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, reg := newManagerOver(t, root, nil)
	mgr.Scan()

	events = gateway.NewUIEvents()
	// The line from main.go. Reproduced rather than imported because main is
	// not importable — which is exactly why nothing tested it.
	reg.OnChange(func(*pluginhost.Snapshot) { events.Publish("registry.changed") })

	h := &gateway.GatewayHandler{Plugins: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)
	mux.HandleFunc(gateway.UIEventsPath, events.Handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, mgr, events
}

// waitEvent waits for one event name, or fails.
func waitEvent(t *testing.T, stream <-chan string, what string) string {
	t.Helper()
	select {
	case name := <-stream:
		return name
	case <-time.After(5 * time.Second):
		t.Fatalf("no event within 5s after %s; an open console would keep showing the "+
			"old menu until somebody reloaded the page", what)
		return ""
	}
}

// Enabling a plugin tells every open console, and the menu it then fetches
// contains the plugin.
func TestEnablingTellsAnOpenConsole(t *testing.T) {
	url, mgr, events := consoleStack(t)
	ctx := context.Background()

	stream, closeStream := openStream(t, url)
	defer closeStream()
	waitForSubscriber(t, events)

	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	name := waitEvent(t, stream, "enabling a plugin")
	t.Logf("console was told: %q", name)

	// The event is only useful if what it prompts a refetch of has changed.
	// An event that fires against an unchanged menu is a wasted round trip;
	// a changed menu nobody was told about is a stale page.
	got := fetchApps(t, url)
	if _, found := findMenu(got.Menu, "/echo"); !found {
		t.Error("the console was told to refresh and the menu it fetched does not have " +
			"the plugin that was just enabled")
	}
}

// Disabling does the same in the other direction, which is the half an
// operator notices: a menu entry that outlives its plugin is a dead link.
func TestDisablingTellsAnOpenConsole(t *testing.T) {
	url, mgr, events := consoleStack(t)
	ctx := context.Background()

	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	stream, closeStream := openStream(t, url)
	defer closeStream()
	waitForSubscriber(t, events)

	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	waitEvent(t, stream, "disabling a plugin")

	got := fetchApps(t, url)
	if _, found := findMenu(got.Menu, "/echo"); found {
		t.Error("the menu still lists a disabled plugin; the console would show a dead link")
	}
}

// An upgrade also tells the console. Menus come from the manifest, so a new
// version can add, rename or drop them — a console left on the old tree would
// offer pages the running plugin no longer serves.
func TestUpgradingTellsAnOpenConsole(t *testing.T) {
	url, mgr, events := consoleStack(t)
	ctx := context.Background()

	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	stream, closeStream := openStream(t, url)
	defer closeStream()
	waitForSubscriber(t, events)

	if err := mgr.Upgrade(ctx, "echo"); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	waitEvent(t, stream, "upgrading a plugin")
}

// A console that opened before anything happened is not told anything.
//
// The other direction, and it matters: a stream that emitted on a timer, or on
// every request, would pass all three tests above while telling an operator
// nothing about what actually changed. The console refetches the whole tree on
// each event, so a chatty stream is a chatty console.
func TestNothingIsPublishedWithoutAChange(t *testing.T) {
	url, _, events := consoleStack(t)

	stream, closeStream := openStream(t, url)
	defer closeStream()
	waitForSubscriber(t, events)

	// Ordinary traffic, which must not look like a registry change.
	for range 3 {
		fetchApps(t, url)
	}

	select {
	case name := <-stream:
		t.Errorf("the console was told %q although nothing changed", name)
	case <-time.After(500 * time.Millisecond):
	}
}
