import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Built assets are zipped and pushed to Core, then served from memory under
// /extensions/python_example/. The relative base keeps asset URLs portable.
export default defineConfig({
  base: "/extensions/python_example/",
  plugins: [react()],
  build: {
    outDir: "dist",
    assetsDir: "assets",
  },
});
