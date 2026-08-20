<script setup>
import { ref, watch, onBeforeUnmount } from "vue";
import { useRoute } from "vue-router";
import { loadMicroApp } from "qiankun";
import { registry, resolveEntry } from "../pluginRegistry";

// Mounts whichever micro-frontend the current route maps to.
//
// This uses loadMicroApp rather than qiankun's registerMicroApps because
// registration is permanent: there is no unregister, so a plugin that was
// disabled would keep its route claimed until the page reloaded. Managing the
// lifecycle by hand is what lets a disabled plugin's UI disappear immediately.

const route = useRoute();
const container = ref(null);
const error = ref("");

let instance = null;
// null rather than "": it means "sync has not run yet", which a key cannot
// otherwise express. A route with nothing to mount has the empty key, so
// starting this at "" made the very first sync mistake "nothing to mount" for
// "already showing nothing" and return before saying anything. Opening a
// plugin URL your role cannot see landed on exactly that path and rendered a
// blank page.
let mountedKey = null;

// Which run of sync is the current one.
//
// sync is asynchronous and its guard is not: the key it compares against was
// only written after the mount finished, so a second trigger arriving while
// the first was still unmounting read the old key, passed the guard, and
// mounted the same app again. The reference to the first was then overwritten
// and it was never taken down — a micro-app left running in a container
// nothing points at.
//
// Two triggers landing in one tick is the obvious case, but it does not take
// that: any two within the time an unmount takes will do, which is why
// navigating and an SSE event a millisecond apart was enough.
//
// One guard, not two. An earlier version also claimed the key before yielding,
// on the theory that it stopped a same-target trigger from racing. Removing it
// failed nothing, and the scenarios where it could differ end in the same
// state — so it was doing no work while reading as though it were.
let generation = 0;

async function unmountCurrent() {
  const dying = instance;
  instance = null;
  if (!dying) return;
  try {
    await dying.unmount();
  } catch {
    // A sub-app that throws while unmounting must not block the next mount;
    // qiankun has already detached it from the container by this point.
  }
}

async function sync() {
  const target = resolveEntry(route.path);

  // Re-mounting the same app on every registry tick would flash the UI for no
  // reason, so only act when the target actually changed.
  const key = target ? target.name + "|" + target.entry : "";
  if (key === mountedKey) return;

  const gen = ++generation;

  await unmountCurrent();

  // Superseded while unmounting: the later run owns the container now, and
  // carrying on here would mount underneath it.
  if (gen !== generation) return;

  error.value = "";

  if (!target || !container.value) {
    // No entry for this route. Two ways to get here and the console cannot tell
    // them apart: the plugin was disabled or uninstalled while its page was
    // open, or it declares roles this user does not have — Core filters those
    // out of the menu tree, so the entry never arrives either way. The wording
    // covers both rather than asserting the one that is easier to phrase.
    if (route.path.startsWith("/apps")) {
      error.value = "这个页面不可用：插件可能已停用或卸载，也可能是当前账号的角色看不到它。";
    }
    mountedKey = key;
    return;
  }

  try {
    instance = loadMicroApp(
      { name: target.name, entry: target.entry, container: container.value },
      { sandbox: { experimentalStyleIsolation: true } },
    );
    mountedKey = key;
  } catch (e) {
    error.value = e?.message || String(e);
  }
}

// Two triggers feed the same idempotent function: navigating to another page,
// and the registry changing underneath a page that is already open. The second
// is what makes a disable take effect without the user reloading.
watch(() => route.path, sync, { immediate: true });
watch(() => registry.version, sync);

onBeforeUnmount(unmountCurrent);
</script>

<template>
  <div class="app-view">
    <div v-if="error" class="app-error">{{ error }}</div>
    <div ref="container" class="app-container"></div>
  </div>
</template>

<style scoped>
.app-view {
  height: 100%;
}
.app-container {
  height: 100%;
}
.app-error {
  padding: 24px;
  color: #b45309;
  background: #fffbeb;
  border: 1px solid #fcd34d;
  border-radius: 6px;
  margin: 16px;
}
</style>
