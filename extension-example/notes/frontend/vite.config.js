import { defineConfig } from "vite";
import qiankun from "vite-plugin-qiankun";

// Core serves this build from /plugins/notes/ (gateway.PluginAssetPrefix + key),
// reading the files straight off the package directory. `base` has to match, or
// the asset URLs in index.html point somewhere the asset handler will 404.
//
// The plugin exposes qiankun's lifecycle hooks through vite-plugin-qiankun.
// Doing it by hand does not work here: vite emits <script type="module">, and
// qiankun 2.x executes entry scripts with eval inside its sandbox, which cannot
// run an ES module. The plugin is what rewrites the output into a form the
// sandbox can execute and attach lifecycles from.
//
// The name below is this plugin's own key. It is not the name the host mounts
// under — qiankun derives that from the menu path (`app:/notes`) — so the
// lifecycles are found through the sandbox's latest-set-property fallback
// rather than by name. See core/frontend/src/pluginRegistry.js.
export default defineConfig({
  base: "/plugins/notes/",
  plugins: [qiankun("notes", { useDevMode: true })],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
