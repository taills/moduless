import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { registry, resolveEntry, refresh } from "../src/pluginRegistry";

// The console's routing table, which decides which micro-frontend a URL mounts.
//
// This is the half of "a plugin's pages appear and disappear with it" that
// lives in the browser. Core's half — serving a menu tree that changes — has
// its own tests; nothing checked what the console does with that tree.

function setMenu(menu) {
  registry.menu = menu;
  registry.apps = [];
  registry.version += 1;
}

beforeEach(() => {
  setMenu([]);
});

describe("resolveEntry", () => {
  it("finds the entry for a plugin's own route", () => {
    setMenu([{ path: "/notes", title: "Notes", entry: "/plugins/notes/" }]);

    const got = resolveEntry("/apps/notes");
    expect(got).not.toBeNull();
    expect(got.entry).toBe("/plugins/notes/");
  });

  it("matches a deeper path against its plugin's prefix", () => {
    setMenu([{ path: "/notes", title: "Notes", entry: "/plugins/notes/" }]);

    // A sub-app owns everything under its own path — its internal router
    // handles the rest, so the console must not treat this as unmatched.
    expect(resolveEntry("/apps/notes/archive/2026")?.entry).toBe("/plugins/notes/");
  });

  it("prefers the longest match when a child declares its own entry", () => {
    // Two plugins can legitimately claim nested paths. Picking the shorter one
    // would mount the parent's micro-frontend for the child's page — the wrong
    // application, rendered without an error.
    setMenu([
      { path: "/reports", title: "Reports", entry: "/plugins/reports/" },
      { path: "/reports/daily", title: "Daily", entry: "/plugins/daily/" },
    ]);

    expect(resolveEntry("/apps/reports")?.entry).toBe("/plugins/reports/");
    expect(resolveEntry("/apps/reports/daily")?.entry).toBe("/plugins/daily/");
    expect(resolveEntry("/apps/reports/daily/detail")?.entry).toBe("/plugins/daily/");
  });

  it("searches nested children, not just the top level", () => {
    setMenu([
      {
        path: "/tools",
        title: "Tools",
        children: [{ path: "/tools/inspect", title: "Inspect", entry: "/plugins/inspect/" }],
      },
    ]);

    expect(resolveEntry("/apps/tools/inspect")?.entry).toBe("/plugins/inspect/");
  });

  it("ignores grouping nodes, which have no entry to mount", () => {
    setMenu([
      {
        path: "/tools",
        title: "Tools",
        children: [{ path: "/tools/inspect", title: "Inspect", entry: "/plugins/inspect/" }],
      },
    ]);

    // The parent is organisational. Returning it would have the console try to
    // mount a menu that exists only to hold other menus.
    expect(resolveEntry("/apps/tools")).toBeNull();
  });

  it("returns null once the plugin is gone", () => {
    setMenu([{ path: "/notes", title: "Notes", entry: "/plugins/notes/" }]);
    expect(resolveEntry("/apps/notes")).not.toBeNull();

    // What a disable looks like from the console's side: the tree simply no
    // longer contains it. AppView turns this null into an unmount.
    setMenu([]);
    expect(resolveEntry("/apps/notes")).toBeNull();
  });

  it("does not match a plugin whose path is a prefix of another word", () => {
    setMenu([{ path: "/note", title: "Note", entry: "/plugins/note/" }]);

    // "/notes" starts with "/note" as a string but is a different route. A
    // plain startsWith would mount the wrong plugin here.
    expect(resolveEntry("/apps/notes")).toBeNull();
  });

  it("gives each mount point a stable, distinct name", () => {
    setMenu([
      { path: "/a", title: "A", entry: "/plugins/a/" },
      { path: "/b", title: "B", entry: "/plugins/b/" },
    ]);

    const a = resolveEntry("/apps/a");
    const b = resolveEntry("/apps/b");

    // qiankun keys its sandbox by name: a collision would have two plugins
    // share one sandbox, and an unstable name would leak a sandbox per
    // navigation.
    expect(a.name).not.toBe(b.name);
    expect(resolveEntry("/apps/a").name).toBe(a.name);
  });
});

describe("refresh", () => {
  const originalFetch = globalThis.fetch;

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it("replaces the registry and bumps the version watchers depend on", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        apps: [{ key: "notes", display_name: "Notes" }],
        menu: [{ path: "/notes", title: "Notes", entry: "/plugins/notes/" }],
      }),
    }));

    const before = registry.version;
    await refresh();

    expect(registry.apps).toHaveLength(1);
    expect(registry.menu).toHaveLength(1);
    // Components watch version to re-evaluate what they should show. Without
    // the bump, an open page would not notice a plugin disappearing.
    expect(registry.version).toBeGreaterThan(before);
  });

  it("treats a response with no plugins as an empty registry", async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({}),
    }));

    await refresh();
    expect(registry.apps).toEqual([]);
    expect(registry.menu).toEqual([]);
  });
});
