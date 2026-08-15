import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { registry } from "../src/pluginRegistry";

// The console mounts a plugin's micro-frontend by hand rather than registering
// it with qiankun, because registration is permanent — there is no unregister,
// so a disabled plugin would keep its route until the page reloaded. Doing the
// lifecycle by hand is what makes "the page disappears when the plugin is
// disabled" true, and this is where that happens.

// A stand-in for qiankun. Recording mounts and unmounts is the whole point:
// what matters is not what the sub-app renders but whether it was taken down.
const mounted = [];
const unmounted = [];
let nextUnmountThrows = false;

vi.mock("qiankun", () => ({
  loadMicroApp: (opts) => {
    mounted.push(opts.name);
    return {
      unmount: async () => {
        if (nextUnmountThrows) {
          nextUnmountThrows = false;
          throw new Error("sub-app blew up on the way out");
        }
        unmounted.push(opts.name);
      },
    };
  },
}));

// One mutable route the tests drive, standing in for vue-router.
const route = { path: "/apps/notes" };
vi.mock("vue-router", () => ({
  useRoute: () => route,
}));

const { default: AppView } = await import("../src/views/AppView.vue");

function setMenu(menu) {
  registry.menu = menu;
  registry.version += 1;
}

const notesMenu = [{ path: "/notes", title: "Notes", entry: "/plugins/notes/" }];

beforeEach(() => {
  mounted.length = 0;
  unmounted.length = 0;
  nextUnmountThrows = false;
  route.path = "/apps/notes";
  setMenu(notesMenu);
});

describe("AppView", () => {
  it("mounts the micro-frontend for the current route", async () => {
    const wrapper = mount(AppView);
    await flushPromises();

    expect(mounted).toHaveLength(1);
    wrapper.unmount();
  });

  it("unmounts when the plugin disappears from the registry", async () => {
    const wrapper = mount(AppView);
    await flushPromises();
    expect(mounted).toHaveLength(1);

    // What a disable looks like here: Core's SSE event triggers a refresh, the
    // tree comes back without this plugin, and the version bump reaches the
    // open page.
    setMenu([]);
    await flushPromises();

    expect(unmounted).toHaveLength(1);
    expect(wrapper.text()).toContain("已被禁用");
    wrapper.unmount();
  });

  it("does not remount on a registry change that did not affect it", async () => {
    const wrapper = mount(AppView);
    await flushPromises();
    expect(mounted).toHaveLength(1);

    // Another plugin was enabled. This page's target is unchanged, so
    // remounting would flash the UI and lose whatever state the sub-app held.
    setMenu([...notesMenu, { path: "/other", title: "Other", entry: "/plugins/other/" }]);
    await flushPromises();

    expect(mounted).toHaveLength(1);
    expect(unmounted).toHaveLength(0);
    wrapper.unmount();
  });

  it("swaps applications when the route moves to another plugin", async () => {
    const wrapper = mount(AppView);
    await flushPromises();

    setMenu([...notesMenu, { path: "/other", title: "Other", entry: "/plugins/other/" }]);
    route.path = "/apps/other";
    await flushPromises();

    expect(mounted).toHaveLength(2);
    // The old one must come down first; two sandboxes on one container is how
    // qiankun ends up with orphaned DOM.
    expect(unmounted).toHaveLength(1);
    wrapper.unmount();
  });

  it("mounts the replacement even if the outgoing app throws on unmount", async () => {
    const wrapper = mount(AppView);
    await flushPromises();

    nextUnmountThrows = true;
    setMenu([...notesMenu, { path: "/other", title: "Other", entry: "/plugins/other/" }]);
    route.path = "/apps/other";
    await flushPromises();

    // A sub-app that fails on the way out must not strand the console on a
    // blank page — a plugin's bug would otherwise break navigation for the
    // whole site.
    expect(mounted).toHaveLength(2);
    wrapper.unmount();
  });

  it("takes the application down when the page itself is closed", async () => {
    const wrapper = mount(AppView);
    await flushPromises();

    wrapper.unmount();
    await flushPromises();

    // Otherwise every navigation away leaks a qiankun sandbox.
    expect(unmounted).toHaveLength(1);
  });
});
