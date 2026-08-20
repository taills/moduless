import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// The host app is served by Core at the web root. In dev (port 7000) we proxy
// /api and /plugins to a running Core so qiankun can load sub-apps and the
// auth/menu endpoints work without CORS.
//
// /plugins is where plugin micro-frontends live (gateway.PluginAssetPrefix).
// This used to say /extensions, which was the reverse-tunnel era's path and had
// been dead since it was removed — invisible until the examples grew pages.
export default defineConfig({
  base: "/",
  plugins: [vue()],
  server: {
    port: 7000,
    proxy: {
      "/api": "http://localhost:80",
      "/plugins": "http://localhost:80",
    },
  },
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
