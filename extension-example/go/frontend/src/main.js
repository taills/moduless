// Minimal vanilla micro-frontend for the Go extension example. Renders the
// /info payload served through the Core gateway tunnel.
async function render() {
  const root = document.getElementById("app");
  root.innerHTML = "<h2>Go 扩展示例</h2><pre>loading...</pre>";
  try {
    const res = await fetch("/api/extensions/go_example/info");
    const data = await res.json();
    root.querySelector("pre").textContent = JSON.stringify(data, null, 2);
  } catch (e) {
    root.querySelector("pre").textContent = "offline: " + e;
  }
}

// Qiankun lifecycle hooks.
export async function bootstrap() {}
export async function mount() {
  await render();
}
export async function unmount() {}

if (!window.__POWERED_BY_QIANKUN__) {
  render();
}
