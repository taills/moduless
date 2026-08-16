import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

// Enabling, disabling and reloading a plugin from the list.
//
// The nine tests next door are all about what the list renders. Nothing ever
// called act, which is the half that starts and stops OS processes — and it
// carries a guard of exactly the shape that turned out to be broken in
// AppView: state set around an await, holding the UI together.
//
// The one with the worst failure is not the double-click. It is the clearing:
// if busy were released only on success, a single failed disable would leave
// every button on the page dead until somebody reloaded, with an error message
// explaining the wrong thing.

let plugins = [];
let apiCalls = [];
let apiHandler = null;

vi.mock("../src/api", () => ({
  api: (path, opts) => {
    apiCalls.push({ path, method: opts?.method || "GET" });
    if (apiHandler) return apiHandler(path, opts);
    return Promise.resolve({ plugins });
  },
}));

let registryRefreshes = 0;
vi.mock("../src/pluginRegistry", () => ({
  refresh: () => {
    registryRefreshes += 1;
    return Promise.resolve();
  },
}));

const { default: Plugins } = await import("../src/views/Plugins.vue");

function plugin(over = {}) {
  return {
    key: "widget",
    display_name: "Widget",
    version: "1.0.0",
    enabled: true,
    replicas: 2,
    ready: 2,
    in_flight: 0,
    permissions: [],
    ...over,
  };
}

async function list(...rows) {
  plugins = rows;
  const wrapper = mount(Plugins);
  await flushPromises();
  return wrapper;
}

function buttonSaying(wrapper, label) {
  return wrapper.findAll("button").find((b) => b.text() === label);
}

beforeEach(() => {
  plugins = [];
  apiCalls = [];
  apiHandler = null;
  registryRefreshes = 0;
});

describe("acting on a plugin", () => {
  it("posts the action and refreshes the menu without waiting for the push", async () => {
    const wrapper = await list(plugin());
    apiCalls = [];

    await buttonSaying(wrapper, "停用").trigger("click");
    await flushPromises();

    expect(apiCalls).toEqual([
      { path: "/system/plugins/widget/disable", method: "POST" },
    ]);
    // Core also announces this over SSE. Refreshing here is what makes the
    // menu change feel immediate to whoever clicked, rather than a moment
    // later for no visible reason.
    expect(registryRefreshes).toBe(1);
  });

  it("locks the other buttons while an action is in flight", async () => {
    const wrapper = await list(plugin());

    let release;
    apiHandler = () =>
      new Promise((r) => {
        release = () => r({ plugins });
      });

    await buttonSaying(wrapper, "停用").trigger("click");
    await flushPromises();

    // Every action is disabled, not just the one clicked: these start and stop
    // OS processes, and two at once on the same plugin is a race an operator
    // should not be able to start by double-clicking.
    for (const b of wrapper.findAll("button")) {
      expect(b.attributes("disabled")).toBeDefined();
    }

    release();
    await flushPromises();
    expect(buttonSaying(wrapper, "停用").attributes("disabled")).toBeUndefined();
  });

  it("frees the buttons again when the action fails", async () => {
    const wrapper = await list(plugin());
    apiHandler = () => Promise.reject(new Error("core said no"));

    await buttonSaying(wrapper, "停用").trigger("click");
    await flushPromises();

    expect(wrapper.text()).toContain("core said no");
    // The failure the guard has to survive: released only on success, one
    // transient error leaves the page dead until a reload, and the message on
    // screen explains something else entirely.
    expect(buttonSaying(wrapper, "停用").attributes("disabled")).toBeUndefined();
  });

  it("keeps showing the plugins when an action fails", async () => {
    const wrapper = await list(plugin());
    apiHandler = () => Promise.reject(new Error("core said no"));

    await buttonSaying(wrapper, "停用").trigger("click");
    await flushPromises();

    // A failed disable must not blank the list: an operator who cannot see the
    // other plugins cannot tell what else is affected.
    expect(wrapper.text()).toContain("Widget");
  });

  it("does not touch the menu when the action failed", async () => {
    const wrapper = await list(plugin());
    apiHandler = () => Promise.reject(new Error("core said no"));

    await buttonSaying(wrapper, "停用").trigger("click");
    await flushPromises();

    expect(registryRefreshes).toBe(0);
  });
});
