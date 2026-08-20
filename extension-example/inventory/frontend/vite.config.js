import { defineConfig } from "vite";
import qiankun from "vite-plugin-qiankun";

// See notes/frontend/vite.config.js for why base must match Core's asset path
// and why the qiankun plugin is required rather than hand-written lifecycles.
export default defineConfig({
  base: "/plugins/inventory/",
  plugins: [qiankun("inventory", { useDevMode: true })],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
