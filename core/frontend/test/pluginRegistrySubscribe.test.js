import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { registry, subscribe } from "../src/pluginRegistry";

// The stream that keeps the console from going quietly stale.
//
// A plugin being enabled or disabled changes the menu, and the console learns
// about it here rather than on the next navigation. That is the whole of
// requirement 4's "without a refresh": the registry replacement and the
// mounting are tested elsewhere, and this is the wire between them.
//
// The failure it guards against is not an error message. A console whose
// stream has quietly died looks exactly like one with nothing to report — it
// shows the menu it had, and an operator who has just disabled a plugin
// watches its pages carry on working.

class FakeEventSource {
  static instances = [];

  constructor(url, opts) {
    this.url = url;
    this.opts = opts;
    this.listeners = {};
    this.closed = false;
    FakeEventSource.instances.push(this);
  }

  addEventListener(name, fn) {
    this.listeners[name] = fn;
  }

  close() {
    this.closed = true;
  }

  // Helpers the tests drive it with.
  emit(name) {
    this.listeners[name]?.();
  }
  fail() {
    this.onerror?.();
  }
  connect() {
    this.onopen?.();
  }
}

let fetchCalls;

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.useFakeTimers();

  fetchCalls = 0;
  vi.stubGlobal("fetch", async () => {
    fetchCalls += 1;
    return {
      ok: true,
      json: async () => ({ menu: [], apps: [] }),
    };
  });
  registry.connected = false;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("subscribe", () => {
  it("opens the stream with credentials, so the session cookie goes along", () => {
    const stop = subscribe();
    try {
      expect(FakeEventSource.instances).toHaveLength(1);
      const [es] = FakeEventSource.instances;
      expect(es.url).toBe("/api/system/ui/events");
      expect(es.opts).toEqual({ withCredentials: true });
    } finally {
      stop();
    }
  });

  it("refetches the registry when Core says it changed", async () => {
    const stop = subscribe();
    try {
      const [es] = FakeEventSource.instances;
      expect(fetchCalls).toBe(0);

      es.emit("registry.changed");
      await vi.runAllTimersAsync();

      expect(fetchCalls).toBe(1);
    } finally {
      stop();
    }
  });

  it("reports the connection so the console can say it is live", () => {
    const stop = subscribe();
    try {
      const [es] = FakeEventSource.instances;
      es.connect();
      expect(registry.connected).toBe(true);

      es.fail();
      expect(registry.connected).toBe(false);
    } finally {
      stop();
    }
  });

  it("reopens after an error rather than sitting on a dead stream", async () => {
    const stop = subscribe();
    try {
      const [first] = FakeEventSource.instances;
      first.fail();
      expect(first.closed).toBe(true);

      await vi.advanceTimersByTimeAsync(5000);

      expect(FakeEventSource.instances).toHaveLength(2);
      expect(FakeEventSource.instances[1].closed).toBe(false);
    } finally {
      stop();
    }
  });

  // Named for what it pins, which is that no new stream opens — not for how.
  // Two guards make that true, and only one of them is load-bearing: removing
  // the clearTimeout leaves this passing, because the reopen checks the closed
  // flag first. The pending timer still fires, and does nothing.
  it("opens nothing more once unsubscribed, even with a retry pending", async () => {
    const stop = subscribe();
    const [first] = FakeEventSource.instances;

    first.fail(); // schedules a reopen
    stop(); // and then the console goes away

    await vi.advanceTimersByTimeAsync(30000);

    expect(FakeEventSource.instances).toHaveLength(1);
    expect(registry.connected).toBe(false);
  });

  it("survives a refetch that fails, because the next event tries again", async () => {
    vi.stubGlobal("fetch", async () => {
      fetchCalls += 1;
      throw new Error("network");
    });

    const stop = subscribe();
    try {
      const [es] = FakeEventSource.instances;

      es.emit("registry.changed");
      await vi.runAllTimersAsync();
      es.emit("registry.changed");
      await vi.runAllTimersAsync();

      // Two attempts, and no unhandled rejection taking the page down with it.
      expect(fetchCalls).toBe(2);
    } finally {
      stop();
    }
  });
});
