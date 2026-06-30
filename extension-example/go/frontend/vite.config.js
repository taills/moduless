import { defineConfig } from "vite";

export default defineConfig({
  base: "/extensions/go_example/",
  build: { outDir: "dist", assetsDir: "assets" },
});
