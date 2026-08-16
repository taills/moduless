import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { reactive } from "vue";
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
// When set, unmount blocks on this until a test releases it — the only way to
// hold a sync mid-flight and let another one start, which is where the
// cross-flush race lives.
let holdUnmount = null;

vi.mock("qiankun", () => ({
  loadMicroApp: (opts) => {
    mounted.push(opts.name);
    return {
      unmount: async () => {
        if (nextUnmountThrows) {
          nextUnmountThrows = false;
          throw new Error("sub-app blew up on the way out");
        }
        if (holdUnmount) {
          const gate = holdUnmount;
          holdUnmount = null;
          await gate;
        }
        unmounted.push(opts.name);
      },
    };
  },
}));

// One mutable route the tests drive, standing in for vue-router.
//
// Reactive, because the real useRoute() is. With a plain object the component's
// route watcher never fires at all, and every route-driven case here was
// actually being driven by the registry watcher happening to read the new path
// — so the line `watch(() => route.path, sync)` had no coverage whatsoever.
const route = reactive({ path: "/apps/notes" });
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
  holdUnmount = null;
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

// Both triggers firing before either finishes.
//
// The guard against redundant work is a synchronous check at the top of an
// async function, and the key it compares against is only written after the
// mount completes. So two triggers arriving close together — which is the
// documented case, a plugin disabled while the user is also navigating — can
// both pass it.
//
// What that costs is not a flicker. The second mount overwrites the reference
// to the first, so the first is never unmounted: a micro-app left running in a
// container nothing points at, for as long as the console stays open.
describe("AppView under two triggers at once", () => {
  // The other half of the race, and it only exists across flushes.
  //
  // Vue coalesces changes from one source within a flush, and both watchers
  // call the same sync, which reads the current route — so two triggers in one
  // tick always compute the same target. Different targets need the first sync
  // to be genuinely in flight when the second starts, which is what holding
  // the unmount arranges.
  //
  // The first claimed "other" and yielded; the second claims "third" and
  // mounts it. When the first resumes it must notice it has been superseded,
  // or it mounts "other" on top and the container shows the wrong plugin.
  it("does not mount an app that a later trigger already replaced", async () => {
    const wrapper = mount(AppView);
    await flushPromises();

    setMenu([
      { path: "/notes", title: "Notes", entry: "/plugins/notes/" },
      { path: "/other", title: "Other", entry: "/plugins/other/" },
      { path: "/third", title: "Third", entry: "/plugins/third/" },
    ]);

    // Hold the unmount of "notes", so the run that targets "other" stops
    // halfway.
    let release;
    holdUnmount = new Promise((r) => {
      release = r;
    });
    route.path = "/apps/other";
    await flushPromises();

    // A second navigation while the first is still parked.
    route.path = "/apps/third";
    await flushPromises();

    release();
    await flushPromises();

    const live = mounted.filter((n) => !unmounted.includes(n));
    expect(live).toHaveLength(1);
    expect(live[0]).toContain("third");

    wrapper.unmount();
    await flushPromises();
    expect([...unmounted].sort()).toEqual([...mounted].sort());
  });

  it("mounts once when the route and the registry change together", async () => {
    const wrapper = mount(AppView);
    await flushPromises();
    expect(mounted).toHaveLength(1);

    // Navigate and change the registry in the same tick, without letting the
    // first settle in between. Counted in totals rather than resetting the
    // arrays, because an unmount recorded here may belong to a mount from
    // before the reset — which is how the first version of this test managed
    // to accuse the code of a leak it had not committed.
    route.path = "/apps/other";
    setMenu([
      { path: "/notes", title: "Notes", entry: "/plugins/notes/" },
      { path: "/other", title: "Other", entry: "/plugins/other/" },
    ]);
    await flushPromises();

    // Two in total: the first app, and the one that replaced it. A third would
    // be the same app mounted twice, with the earlier reference overwritten.
    expect(mounted).toHaveLength(2);
    expect(mounted[1]).toContain("other");
    expect(unmounted).toHaveLength(1);

    wrapper.unmount();
    await flushPromises();

    // Everything that went up came down. A mount with no matching unmount is
    // an app the console can no longer reach and never stops.
    expect([...unmounted].sort()).toEqual([...mounted].sort());
  });
});
