import { registerMicroApps, start } from "qiankun";
import { api } from "./api";

let started = false;
const registered = new Set();

// setupMicroApps fetches the registered extensions from Core and wires each as a
// qiankun micro-app mounted into #subapp-container under /apps/<key>. It is safe
// to call repeatedly: new apps are registered and start() runs only once.
export async function setupMicroApps() {
  const apps = await api("/system/ui/apps");

  const fresh = apps.filter((a) => !registered.has(a.key));
  if (fresh.length) {
    registerMicroApps(
      fresh.map((a) => {
        registered.add(a.key);
        return {
          name: a.key,
          entry: a.entry,
          container: "#subapp-container",
          activeRule: "/apps/" + a.key,
        };
      })
    );
  }

  if (!started) {
    start({ sandbox: { experimentalStyleIsolation: true } });
    started = true;
  }

  return apps;
}
