import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

// The plugin list, which is where an operator finds out what is wrong.
//
// The states it distinguishes are the point. A plugin whose circuit breaker
// has tripped is enabled, has its replicas, and is failing every call — from
// the outside identical to one that is merely slow, unless the list says
// otherwise. And a plugin restarting every thirty seconds reads as "running"
// on every refresh unless something says how long it has actually been up.

let plugins = [];

vi.mock("../src/api", () => ({
  api: () => Promise.resolve({ plugins }),
}));
vi.mock("../src/pluginRegistry", () => ({
  refresh: () => Promise.resolve(),
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

beforeEach(() => {
  plugins = [];
});

describe("Plugins list", () => {
  it("shows a healthy plugin as running", async () => {
    const wrapper = await list(plugin());
    expect(wrapper.text()).toContain("运行中");
  });

  it("says a plugin is tripped rather than running", async () => {
    // Core has stopped calling it. That clears on its own and needs no
    // intervention, which is the opposite of every other unhealthy state —
    // so reading it as "running" sends an operator looking in the wrong place.
    const wrapper = await list(plugin({ tripped: 1 }));

    expect(wrapper.text()).toContain("熔断");
    expect(wrapper.text()).not.toContain("运行中");
  });

  it("prefers tripped over partially ready, since it is the more specific fault", async () => {
    const wrapper = await list(plugin({ ready: 1, tripped: 1 }));
    expect(wrapper.text()).toContain("熔断");
  });

  it("still shows a disabled plugin as disabled, tripped or not", async () => {
    const wrapper = await list(plugin({ enabled: false, tripped: 1 }));
    expect(wrapper.text()).toContain("已停用");
  });

  it("shows a load error above everything else", async () => {
    const wrapper = await list(plugin({ load_error: "manifest: version is required" }));
    expect(wrapper.text()).toContain("加载失败");
    expect(wrapper.text()).toContain("version is required");
  });

  it("reports how long the oldest replica has been up", async () => {
    const started = new Date(Date.now() - 90 * 1000).toISOString();
    const wrapper = await list(plugin({ oldest_started_at: started }));
    expect(wrapper.text()).toContain("已运行");
    expect(wrapper.text()).toContain("分钟");
  });

  it("says nothing about uptime when Core did not report it", async () => {
    const wrapper = await list(plugin());
    expect(wrapper.text()).not.toContain("已运行");
  });

  it("says nothing about uptime when Core reports a zero timestamp", async () => {
    // What a zero time.Time serialises to. Core no longer sends it, but this
    // is what turned a plugin that had never started into "已运行 739847 天".
    const wrapper = await list(plugin({ oldest_started_at: "0001-01-01T00:00:00Z" }));
    expect(wrapper.text()).not.toContain("已运行");
  });

  it("shows a queue backlog and, separately, what the queue gave up on", async () => {
    // Separately on purpose: a backlog drains and a graveyard does not, and
    // one number for both would make them indistinguishable.
    const wrapper = await list(plugin({ queue_depth: 120, queue_dead: 4 }));
    expect(wrapper.text()).toContain("队列积压 120");
    expect(wrapper.text()).toContain("已放弃 4");
  });

  it("offers a config button only to plugins that declare settings", async () => {
    const withConfig = await list(plugin({ config: [{ key: "greeting" }] }));
    expect(withConfig.text()).toContain("配置");

    const without = await list(plugin());
    expect(without.text()).not.toContain("配置");
  });
});
