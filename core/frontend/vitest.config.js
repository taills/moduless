import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig({
  plugins: [vue()],
  test: {
    // jsdom rather than node: the console's code touches localStorage, cookies
    // and the DOM, and testing it in an environment without them would only
    // prove that the mocks work.
    environment: "jsdom",
    include: ["test/**/*.test.js"],
  },
});
