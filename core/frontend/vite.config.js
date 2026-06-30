import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// The host app is served by Core at the web root. In dev (port 7000) we proxy
// /api and /extensions to a running Core so qiankun can load sub-apps and the
// auth/menu endpoints work without CORS.
export default defineConfig({
  base: "/",
  plugins: [vue()],
  server: {
    port: 7000,
    proxy: {
      "/api": "http://localhost:80",
      "/extensions": "http://localhost:80",
    },
  },
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
