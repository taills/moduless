// The inventory plugin's console page.
//
// The one with real write paths: reserve and restock both go through optimistic
// concurrency inside the plugin, so they can come back 409 or 503 when they lose
// a race. Those are normal outcomes here, not crashes — the page says what
// happened and leaves the row alone.
//
// Plain DOM and a locally duplicated api()/escapeHtml are deliberate; see
// notes/frontend/src/main.js for why an example plugin does not share them.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/inventory";

async function api(path, options = {}) {
  const res = await fetch(API + path, {
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    ...options,
  });

  if (!res.ok) {
    const body = await res.text();
    let message = body || res.statusText;
    if ((res.headers.get("content-type") || "").includes("json")) {
      try {
        message = JSON.parse(body).error || message;
      } catch {
        // Claimed JSON, was not. Keep the raw body.
      }
    }
    const err = new Error(message);
    err.status = res.status;
    throw err;
  }

  return res.status === 204 ? null : res.json();
}

function escapeHtml(s) {
  return String(s ?? "").replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c],
  );
}

const state = { items: [], next: "" };
let root = null;

function renderShell() {
  root.innerHTML = `
    <style>
      .inv-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
      .inv-page h2 { margin: 0 0 4px; font-size: 22px; }
      .inv-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
      .inv-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .inv-page .ok { color: #15803d; background: #f0fdf4; border: 1px solid #bbf7d0;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .inv-page form { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 16px; }
      .inv-page input { padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 6px; font-size: 14px; }
      .inv-page input[type="number"] { width: 110px; }
      .inv-page button { padding: 8px 14px; border: 1px solid #d1d5db; background: #fff;
        border-radius: 6px; cursor: pointer; font-size: 14px; }
      .inv-page button.primary { background: #2563eb; border-color: #2563eb; color: #fff; }
      .inv-page table { width: 100%; border-collapse: collapse; font-size: 14px; }
      .inv-page th, .inv-page td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #e5e7eb; }
      .inv-page th { color: #6b7280; font-weight: 500; font-size: 13px; }
      .inv-page td.sku { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
      .inv-page td.num { text-align: right; font-variant-numeric: tabular-nums; }
      .inv-page .low { color: #b45309; font-weight: 600; }
      .inv-page .act { display: flex; gap: 6px; }
      .inv-page .act input { width: 66px; padding: 5px 7px; }
      .inv-page .act button { padding: 5px 10px; font-size: 13px; }
      .inv-page .empty { text-align: center; color: #9ca3af; padding: 24px 0; }
    </style>
    <div class="inv-page">
      <h2>库存</h2>
      <p class="sub">预留和补货走乐观并发，冲突时会被拒绝并要求重试 —— 这是正常结果，不是故障。</p>
      <div class="err" id="err"></div>
      <div class="ok" id="ok"></div>
      <form id="upsert">
        <input name="sku" placeholder="SKU（必填）" required />
        <input name="name" placeholder="名称" />
        <input name="on_hand" type="number" min="0" placeholder="现货" required />
        <input name="reserved" type="number" min="0" placeholder="已预留" value="0" />
        <button type="submit" class="primary">写入</button>
      </form>
      <table>
        <thead><tr><th>SKU</th><th>名称</th><th class="num">现货</th><th class="num">已预留</th><th class="num">可用</th><th>操作</th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
      <div style="margin-top:12px"><button id="more" style="display:none">加载更多</button></div>
    </div>`;

  root.querySelector("#upsert").addEventListener("submit", onUpsert);
  root.querySelector("#more").addEventListener("click", () => run(() => load(false)));
}

function renderList() {
  const rows = root.querySelector("#rows");
  if (!rows) return;

  if (state.items.length === 0) {
    rows.innerHTML = `<tr><td colspan="6" class="empty">还没有库存项</td></tr>`;
  } else {
    rows.innerHTML = state.items
      .map((it) => {
        const available = (it.on_hand || 0) - (it.reserved || 0);
        return `<tr>
          <td class="sku">${escapeHtml(it.sku)}</td>
          <td>${escapeHtml(it.name) || '<span style="color:#9ca3af">—</span>'}</td>
          <td class="num">${escapeHtml(it.on_hand ?? 0)}</td>
          <td class="num">${escapeHtml(it.reserved ?? 0)}</td>
          <td class="num ${available <= 0 ? "low" : ""}">${escapeHtml(available)}</td>
          <td>
            <div class="act">
              <input type="number" min="1" value="1" data-qty="${escapeHtml(it.sku)}" />
              <button data-reserve="${escapeHtml(it.sku)}">预留</button>
              <button data-restock="${escapeHtml(it.sku)}">补货</button>
            </div>
          </td>
        </tr>`;
      })
      .join("");

    rows.querySelectorAll("[data-reserve]").forEach((b) =>
      b.addEventListener("click", () => act(b.getAttribute("data-reserve"), "reserve")),
    );
    rows.querySelectorAll("[data-restock]").forEach((b) =>
      b.addEventListener("click", () => act(b.getAttribute("data-restock"), "restock")),
    );
  }

  const more = root.querySelector("#more");
  if (more) more.style.display = state.next ? "" : "none";
}

function qtyFor(sku) {
  const input = root.querySelector(`[data-qty="${CSS.escape(sku)}"]`);
  return Math.max(1, Number(input?.value) || 1);
}

async function act(sku, kind) {
  const qty = qtyFor(sku);
  await run(async () => {
    const body = kind === "reserve" ? { qty, for: "console" } : { qty };
    const result = await api(`/items/${encodeURIComponent(sku)}/${kind}`, {
      method: "POST",
      body: JSON.stringify(body),
    });

    const ok = root.querySelector("#ok");
    ok.textContent =
      kind === "reserve"
        ? `已预留 ${qty} 件 ${sku}，剩余可用 ${result.remaining}`
        : `已补货 ${qty} 件 ${sku}，现货 ${result.on_hand}`;
    ok.style.display = "block";

    state.next = "";
    await load(true);
  });
}

// PUT is a whole-item upsert, not a delta — on_hand and reserved are written
// exactly as given. reserve/restock are the incremental paths.
async function onUpsert(e) {
  e.preventDefault();
  const form = e.target;
  const sku = form.sku.value.trim();
  if (!sku) return;

  await run(async () => {
    await api(`/items/${encodeURIComponent(sku)}`, {
      method: "PUT",
      body: JSON.stringify({
        sku,
        name: form.name.value.trim(),
        on_hand: Number(form.on_hand.value) || 0,
        reserved: Number(form.reserved.value) || 0,
      }),
    });
    form.reset();
    form.reserved.value = "0";
    state.next = "";
    await load(true);
    root.querySelector("#ok").style.display = "none";
  });
}

async function load(reset = true) {
  const params = new URLSearchParams();
  if (!reset && state.next) params.set("after", state.next);
  const qs = params.toString();

  const data = await api(qs ? `/items?${qs}` : "/items");
  state.items = reset ? data.items || [] : state.items.concat(data.items || []);
  state.next = data.next || "";
  renderList();
}

async function run(fn) {
  const err = root.querySelector("#err");
  try {
    await fn();
    if (err) err.style.display = "none";
  } catch (e) {
    if (!err) return;
    // 409 and 503 here mean "someone else got there first" — worth wording as
    // retryable rather than dumping the raw message.
    err.textContent =
      e.status === 409 || e.status === 503
        ? `${e.message || "并发冲突"}（重试即可）`
        : e.status === 401
          ? "登录已失效，请重新登录。"
          : e.message || String(e);
    err.style.display = "block";
    root.querySelector("#ok").style.display = "none";
  }
}

function start(container) {
  const host = container || document;
  root = host.querySelector("#inventory-app");
  if (!root) return;
  renderShell();
  run(() => load(true));
}

renderWithQiankun({
  bootstrap() {},
  mount(props) {
    start(props.container);
  },
  unmount(props) {
    const el = (props.container || document).querySelector("#inventory-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
