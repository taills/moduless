import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// Built assets are zipped and pushed to Core, then served from memory under
// /extensions/java_example/. The relative base keeps asset URLs portable.
export default defineConfig({
  base: "/extensions/java_example/",
  plugins: [vue()],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
