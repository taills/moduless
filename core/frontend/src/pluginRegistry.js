import { reactive } from "vue";
import { api } from "./api";

// The console's view of what Core is currently serving.
//
// `version` increments on every refresh. Components watch it to re-evaluate
// what they should be showing, which is how a plugin being enabled or disabled
// reaches a page that is already open.
export const registry = reactive({
  apps: [],
  menu: [],
  version: 0,
  connected: false,
});

// refresh pulls the current apps and the merged menu tree.
//
// Core does the merging now. The console used to run its own copy of the
// merge-by-path algorithm, which meant the same logic lived in two languages
// and could disagree.
export async function refresh() {
  const data = await api("/system/ui/apps");
  registry.apps = data.apps || [];
  registry.menu = data.menu || [];
  registry.version += 1;
}

/**
 * Finds the micro-app that should be mounted for a console route.
 *
 * @param {string} routePath a console path such as "/apps/reports/daily"
 * @returns {{name: string, entry: string, path: string}|null}
 */
export function resolveEntry(routePath) {
  const target = routePath.startsWith("/apps") ? routePath.slice("/apps".length) || "/" : routePath;

  let best = null;
  const walk = (nodes) => {
    for (const node of nodes || []) {
      // Prefer the longest matching path so a child route wins over its
      // parent when both declare an entry.
      if (node.entry && (target === node.path || target.startsWith(node.path + "/"))) {
        if (!best || node.path.length > best.path.length) {
          best = { name: appNameFor(node), entry: node.entry, path: node.path };
        }
      }
      walk(node.children);
    }
  };
  walk(registry.menu);
  return best;
}

// qiankun keys sandboxes by app name, so the name must be stable for a given
// mount point and distinct between them.
function appNameFor(node) {
  return "app:" + node.path;
}

/**
 * Subscribes to Core's change stream.
 *
 * Each event just means "something changed"; the console refetches rather than
 * applying a diff. That costs one small request and cannot drift out of sync,
 * whereas a diff stream has to get ordering, deduplication and reconnection
 * exactly right or it quietly shows the wrong thing.
 *
 * @returns {() => void} unsubscribe
 */
export function subscribe() {
  let source = null;
  let closed = false;
  let retry = null;

  const open = () => {
    if (closed) return;
    // Same-origin, so the session cookie Core sets is sent automatically and
    // no extra auth plumbing is needed.
    source = new EventSource("/api/system/ui/events", { withCredentials: true });

    source.onopen = () => {
      registry.connected = true;
    };
    source.addEventListener("registry.changed", () => {
      refresh().catch(() => {
        // A failed refresh is not fatal: the next event, or the next
        // navigation, will try again.
      });
    });
    source.onerror = () => {
      registry.connected = false;
      source?.close();
      if (closed) return;
      // EventSource reconnects on its own, but not after some proxy errors.
      // Reopening explicitly keeps the console live rather than silently
      // stale, which is the failure this whole stream exists to prevent.
      retry = setTimeout(open, 5000);
    };
  };

  open();

  return () => {
    closed = true;
    registry.connected = false;
    if (retry) clearTimeout(retry);
    source?.close();
  };
}
