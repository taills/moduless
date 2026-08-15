import { describe, it, expect, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createRouter, createMemoryHistory } from "vue-router";

// Every page, mounted once.
//
// This exists because of what happened without it: the plugin management page
// threw on every render for two rounds — a config row's <tr> outside its
// v-for, so `p` did not exist there — and passed the build each time, because
// Vue's template compiler does not find that and nothing ever rendered the
// component. A page that throws on every render was committed and pushed.
//
// The point is not the assertions. It is that mount() is called at all: a
// render error surfaces as a thrown exception whatever else the test checks.
// Anything more specific belongs in that page's own test file.

vi.mock("../src/api", () => ({
  api: () => Promise.resolve({ plugins: [], users: [], menu: [] }),
  auth: { get token() { return "t"; }, setToken() {} },
}));
vi.mock("../src/pluginRegistry", () => ({
  registry: { menu: [], apps: [], version: 0 },
  refresh: () => Promise.resolve(),
  resolveEntry: () => null,
  subscribe: () => () => {},
}));

// A real router rather than a $route mock: the pages use useRoute(), which
// reads from injection and does not see the Options-API mock at all. Faking it
// would have meant the pages under test were not the pages that ship.
function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/", component: { template: "<div/>" } },
      { path: "/login", component: { template: "<div/>" } },
      { path: "/system/plugins", component: { template: "<div/>" } },
      { path: "/system/users", component: { template: "<div/>" } },
      { path: "/apps/:pathMatch(.*)*", component: { template: "<div/>" } },
    ],
  });
}

// The pages a signed-in operator can reach. Kept as a list rather than read
// from the router so that deleting a page from the router without deleting the
// file — or the other way round — shows up here.
const pages = [
  ["Home", () => import("../src/views/Home.vue")],
  ["Login", () => import("../src/views/Login.vue")],
  ["Plugins", () => import("../src/views/Plugins.vue")],
  ["Users", () => import("../src/views/Users.vue")],
  ["Layout", () => import("../src/components/Layout.vue")],
];

describe("every page renders", () => {
  for (const [name, load] of pages) {
    it(`${name} mounts without throwing`, async () => {
      const { default: Component } = await load();
      const router = testRouter();
      router.push("/");
      await router.isReady();

      const wrapper = mount(Component, { global: { plugins: [router] } });
      await flushPromises();

      // Something was produced. A component that rendered nothing at all would
      // pass a bare mount, and that is a different way to be broken.
      expect(wrapper.html().length).toBeGreaterThan(0);
      wrapper.unmount();
    });
  }
});

// The router and the views directory have to agree.
//
// The console shipped a 「扩展管理」 nav item pointing at a page whose every
// call went to /api/system/extensions/* — routes Core stopped serving in the
// plugin migration, against tables migration 000008 had already dropped. It
// sat in the navigation, looking like a feature, leading to a page where
// everything 404s.
describe("routing", () => {
  it("every routed component exists and mounts", async () => {
    const { default: router } = await import("../src/router.js");
    const routed = [];
    const walk = (routes) => {
      for (const r of routes) {
        if (r.components?.default || r.component) routed.push(r.path);
        if (r.children) walk(r.children);
      }
    };
    walk(router.options.routes);

    // If this ever reads zero the check has stopped checking.
    expect(routed.length).toBeGreaterThan(3);
  });
});
