import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createRouter, createMemoryHistory } from "vue-router";
import Layout from "../src/components/Layout.vue";
import { registry } from "../src/pluginRegistry";

// The topbar, rendered rather than reasoned about.
//
// It showed `[ "apikey" ]` where the page name belongs, because it interpolated
// route.params.pathMatch and that param is an array: the route is
// `apps/:pathMatch(.*)*` and the trailing `*` makes it repeatable. A unit test
// on resolveTitle would not have caught it — the bug was in the template, in
// what it chose to read.

vi.mock("../src/api", () => ({
  api: () => Promise.resolve({ apps: [], menu: [] }),
  auth: {
    get token() {
      return "t";
    },
    setToken() {},
  },
}));

// subscribe opens an EventSource, which jsdom does not provide. Everything
// else in the registry — resolveTitle above all — stays real, since that is
// what is under test here.
vi.mock("../src/pluginRegistry", async (importOriginal) => {
  const actual = await importOriginal();
  return { ...actual, refresh: () => Promise.resolve(), subscribe: () => () => {} };
});

function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/", component: { template: "<div/>" } },
      { path: "/login", component: { template: "<div/>" } },
      { path: "/system/plugins", component: { template: "<div/>" } },
      { path: "/apps/:pathMatch(.*)*", component: { template: "<div/>" } },
    ],
  });
}

async function topbarAt(path) {
  const router = testRouter();
  router.push(path);
  await router.isReady();

  const wrapper = mount(Layout, { global: { plugins: [router] } });
  await flushPromises();
  return wrapper;
}

beforeEach(() => {
  registry.menu = [];
  registry.apps = [];
  registry.version += 1;
});

describe("the topbar names the current page", () => {
  it("uses the menu title for a plugin route", async () => {
    registry.menu = [{ path: "/apikey", title: "API 密钥", entry: "/plugins/apikey/" }];

    const wrapper = await topbarAt("/apps/apikey");

    expect(wrapper.find(".crumb").text()).toBe("API 密钥");
    wrapper.unmount();
  });

  it("never renders the route parameter as an array", async () => {
    // No menu at all: the fallback path, which is where the array would show
    // up if the breadcrumb went back to reading params directly.
    const wrapper = await topbarAt("/apps/apikey");

    const crumb = wrapper.find(".crumb").text();
    expect(crumb).not.toContain("[");
    expect(crumb).not.toContain('"');
    wrapper.unmount();
  });

  it("says 概览 on the home route", async () => {
    const wrapper = await topbarAt("/");

    expect(wrapper.find(".crumb").text()).toBe("概览");
    wrapper.unmount();
  });

  it("stays empty on a console page that has its own heading", async () => {
    // Plugins.vue renders <h2>插件管理</h2>; a breadcrumb naming the route as
    // well showed "/system/plugins" stacked above it.
    const wrapper = await topbarAt("/system/plugins");

    expect(wrapper.find(".crumb").text()).toBe("");
    wrapper.unmount();
  });
});
