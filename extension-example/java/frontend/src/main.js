import { createApp } from "vue";
import App from "./App.vue";
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

let app = null;

function render(container) {
  const el = container
    ? container.querySelector("#java-example-root")
    : document.getElementById("java-example-root");
  app = createApp(App);
  app.mount(el);
}

// Qiankun lifecycle (via vite-plugin-qiankun) so the host can mount this app.
renderWithQiankun({
  bootstrap() {},
  mount(props) {
    render(props.container);
  },
  unmount() {
    if (app) {
      app.unmount();
      app = null;
    }
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  render();
}
