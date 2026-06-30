import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import qiankun from "vite-plugin-qiankun";

// Built assets are zipped and pushed to Core, then served from memory under
// /extensions/python_example/. vite-plugin-qiankun exposes the lifecycle hooks
// so the host can load this app as a micro-frontend.
export default defineConfig({
  base: "/extensions/python_example/",
  plugins: [react(), qiankun("python_example", { useDevMode: true })],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});

