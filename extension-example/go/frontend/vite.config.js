import { defineConfig } from "vite";
import qiankun from "vite-plugin-qiankun";

// vite-plugin-qiankun exposes the qiankun lifecycle hooks from the ESM build so
// the host can load this app as a micro-frontend. The app name must match the
// extension key registered with Core.
export default defineConfig({
  base: "/extensions/go_example/",
  plugins: [qiankun("go_example", { useDevMode: true })],
  build: { outDir: "dist", assetsDir: "assets" },
});
