import { createApp } from "vue";
import App from "./App.vue";

let app = null;

function render(container) {
  const el = container
    ? container.querySelector("#java-example-root")
    : document.getElementById("java-example-root");
  app = createApp(App);
  app.mount(el);
}

// Qiankun micro-frontend lifecycle hooks.
export async function bootstrap() {}
export async function mount(props) {
  render(props && props.container);
}
export async function unmount() {
  if (app) {
    app.unmount();
    app = null;
  }
}

if (!window.__POWERED_BY_QIANKUN__) {
  render();
}
