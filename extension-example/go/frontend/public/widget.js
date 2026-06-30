// Dashboard slot widget for the Go extension example. Declared in manifest.yaml
// as ui_slots[].component_entry and served by Core at
// /extensions/go_example/widget.js. Kept dependency-free so it loads as a plain
// ES module in any host shell. Call mount(el); it also self-renders standalone.
const API = "/api/extensions/go_example";

export async function mount(el) {
  el.innerHTML = `<div style="padding:12px;border:1px solid #eee;border-radius:8px;font-family:system-ui">
    <strong>Go Items</strong><div id="go-widget-count" style="font-size:28px">…</div></div>`;
  try {
    const res = await fetch(API + "/items");
    const data = await res.json();
    el.querySelector("#go-widget-count").textContent = String((data.items || []).length);
  } catch {
    el.querySelector("#go-widget-count").textContent = "—";
  }
}

if (!window.__POWERED_BY_QIANKUN__ && document.currentScript) {
  const host = document.createElement("div");
  document.body.appendChild(host);
  mount(host);
}

export default { mount };
