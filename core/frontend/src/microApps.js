import { registerMicroApps, start } from "qiankun";
import { api } from "./api";

let started = false;
const registered = new Set();

// collectEntries walks every extension's menus and yields one entry per leaf
// node (entry != "") with its path. The path becomes the qiankun activeRule so
// each micro-app is mounted under its declared URL rather than under
// /apps/<key>. Duplicates (two extensions registering the same entry) keep
// the first one and emit a console.warn — this is a configuration error.
function collectEntries(apps) {
  const out = [];
  const seen = new Map();
  for (const app of apps) {
    const menus = app.menus || [];
    walk(menus);
  }
  function walk(nodes) {
    for (const n of nodes) {
      if (n.entry) {
        const key = n.entry + "|" + n.path;
        if (seen.has(key)) {
          // eslint-disable-next-line no-console
          console.warn(`[microApps] duplicate entry ${n.entry} for path ${n.path}; ignoring`);
          continue;
        }
        seen.set(key, true);
        out.push({ entry: n.entry, path: n.path, key: app.key + "::" + n.path });
      }
      if (n.children) walk(n.children);
    }
  }
  return out;
}

// mergeMenusByPath combines per-extension menus into a single tree. Same Path
// nodes are merged; the first extension to declare a path wins the title/icon
// (other extensions' title/icon for the same path are ignored). Children are
// recursively merged and sorted ascending by order.
function mergeMenusByPath(apps) {
  let root = [];
  for (const app of apps) {
    for (const node of app.menus || []) {
      mergeInto(root, node);
    }
  }
  sortSiblings(root);
  return root;
}
function mergeInto(siblings, node) {
  const idx = siblings.findIndex((s) => s.path === node.path);
  let target;
  if (idx < 0) {
    // First writer: clone without children (children will be merged below).
    siblings.push({ ...node, children: [] });
    target = siblings[siblings.length - 1];
  } else {
    target = siblings[idx];
    // First writer wins title/icon/roles/order; subsequent writers only
    // contribute children.
  }
  for (const child of node.children || []) {
    mergeInto(target.children, child);
  }
  sortSiblings(target.children);
}
function sortSiblings(nodes) {
  nodes.sort((a, b) => {
    if (a.order !== b.order) return a.order - b.order;
    return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
  });
}

// setupMicroApps fetches the registered extensions from Core, builds the
// merged menu tree, and wires every leaf node as a qiankun micro-app mounted
// under its declared path. Safe to call repeatedly: new entries are
// registered and start() runs only once. Returns the merged menu tree.
export async function setupMicroApps() {
  const apps = await api("/system/ui/apps");

  const entries = collectEntries(apps);
  const fresh = entries.filter((e) => !registered.has(e.key));
  if (fresh.length) {
    registerMicroApps(
      fresh.map((e) => {
        registered.add(e.key);
        return {
          name: e.key,
          entry: e.entry,
          container: "#subapp-container",
          activeRule: "/apps" + e.path,
        };
      }),
    );
  }

  if (!started) {
    start({ sandbox: { experimentalStyleIsolation: true } });
    started = true;
  }

  return {
    apps,
    menu: mergeMenusByPath(apps),
  };
}
