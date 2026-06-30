import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import qiankun from "vite-plugin-qiankun";

// Built assets are zipped and pushed to Core, then served from memory under
// /extensions/java_example/. vite-plugin-qiankun exposes the lifecycle hooks so
// the host can load this app as a micro-frontend.
export default defineConfig({
  base: "/extensions/java_example/",
  plugins: [vue(), qiankun("java_example", { useDevMode: true })],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});

