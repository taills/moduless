// Dashboard slot widget for the Java extension example. Declared in
// manifest.yaml and served by Core at /extensions/java_example/widget.js.
// Dependency-free so it loads as a plain ES module in any host shell.
const API = "/api/extensions/java_example";

export async function mount(el) {
  el.innerHTML = `<div style="padding:12px;border:1px solid #eee;border-radius:8px;font-family:system-ui">
    <strong>Java Items</strong><div id="java-widget-count" style="font-size:28px">…</div></div>`;
  try {
    const res = await fetch(API + "/items");
    const data = await res.json();
    el.querySelector("#java-widget-count").textContent = String((data.items || []).length);
  } catch {
    el.querySelector("#java-widget-count").textContent = "—";
  }
}

if (!window.__POWERED_BY_QIANKUN__ && document.currentScript) {
  const host = document.createElement("div");
  document.body.appendChild(host);
  mount(host);
}

export default { mount };
