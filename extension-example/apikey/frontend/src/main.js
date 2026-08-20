// The apikey plugin's console page.
//
// Every route here is admin-only, checked inside the plugin — the manifest's
// menus[].roles only decides who sees the menu item, which is not the same
// thing and never has been. The page is written so that a non-admin who types
// the URL gets a sentence rather than the raw word "forbidden".
//
// Plain DOM and a locally duplicated api()/escapeHtml are deliberate; see
// notes/frontend/src/main.js for why an example plugin does not share them.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/apikey";

// This plugin answers almost entirely in plain text (http.Error), unlike notes
// which returns JSON. Both shapes are handled; the status rides along so the
// caller can tell "not allowed" from "went wrong".
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

const state = { keys: [] };
let root = null;

function renderShell() {
  root.innerHTML = `
    <style>
      .apikey-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
      .apikey-page h2 { margin: 0 0 4px; font-size: 22px; }
      .apikey-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
      .apikey-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .apikey-page .minted { background: #fffbeb; border: 1px solid #fcd34d; border-radius: 8px;
        padding: 14px 16px; margin-bottom: 16px; display: none; }
      .apikey-page .minted .warn { color: #92400e; font-weight: 600; margin-bottom: 8px; }
      .apikey-page .minted .value { display: flex; gap: 8px; align-items: center; }
      .apikey-page .minted code { flex: 1; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
        font-size: 13px; background: #fff; border: 1px solid #fcd34d; border-radius: 6px;
        padding: 8px 10px; word-break: break-all; }
      .apikey-page form { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 16px; }
      .apikey-page input { padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 6px; font-size: 14px; }
      .apikey-page button { padding: 8px 14px; border: 1px solid #d1d5db; background: #fff;
        border-radius: 6px; cursor: pointer; font-size: 14px; }
      .apikey-page button.primary { background: #2563eb; border-color: #2563eb; color: #fff; }
      .apikey-page table { width: 100%; border-collapse: collapse; font-size: 14px; }
      .apikey-page th, .apikey-page td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #e5e7eb; }
      .apikey-page th { color: #6b7280; font-weight: 500; font-size: 13px; }
      .apikey-page td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
      .apikey-page .role { display: inline-block; font-size: 12px; background: #eef2ff; color: #3730a3;
        border-radius: 4px; padding: 2px 7px; margin-right: 4px; }
      .apikey-page .revoked { color: #9ca3af; text-decoration: line-through; }
      .apikey-page .empty { text-align: center; color: #9ca3af; padding: 24px 0; }
    </style>
    <div class="apikey-page">
      <h2>API 密钥</h2>
      <p class="sub">签发给外部调用方的密钥。Core 只存哈希，明文在签发后无法再取回。</p>
      <div class="err" id="err"></div>
      <div class="minted" id="minted">
        <div class="warn">这是唯一一次显示明文，关闭后无法再取回</div>
        <div class="value">
          <code id="plain"></code>
          <button id="copy">复制</button>
          <button id="dismiss">我已保存</button>
        </div>
      </div>
      <form id="create">
        <input name="user_id" placeholder="用户 ID（必填）" required />
        <input name="name" placeholder="用户名" />
        <input name="label" placeholder="用途备注" />
        <input name="roles" placeholder="角色，逗号分隔" />
        <button type="submit" class="primary">签发</button>
      </form>
      <table>
        <thead><tr><th>备注</th><th>用户</th><th>角色</th><th>哈希</th><th>创建时间</th><th></th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
    </div>`;

  root.querySelector("#create").addEventListener("submit", onCreate);
  root.querySelector("#copy").addEventListener("click", onCopy);
  root.querySelector("#dismiss").addEventListener("click", () => {
    root.querySelector("#plain").textContent = "";
    root.querySelector("#minted").style.display = "none";
  });
}

function renderList() {
  const rows = root.querySelector("#rows");
  if (!rows) return;

  if (state.keys.length === 0) {
    rows.innerHTML = `<tr><td colspan="6" class="empty">还没有签发过密钥</td></tr>`;
    return;
  }

  rows.innerHTML = state.keys
    .map((k) => {
      const roles = (k.roles || []).map((r) => `<span class="role">${escapeHtml(r)}</span>`).join("");
      return `<tr class="${k.revoked ? "revoked" : ""}">
        <td>${escapeHtml(k.label) || '<span style="color:#9ca3af">—</span>'}</td>
        <td>${escapeHtml(k.name || k.user_id)}</td>
        <td>${roles || '<span style="color:#9ca3af">—</span>'}</td>
        <td class="mono" title="${escapeHtml(k.hash)}">${escapeHtml(String(k.hash || "").slice(0, 12))}…</td>
        <td>${escapeHtml(formatTime(k.created))}</td>
        <td>${
          k.revoked
            ? '<span style="color:#9ca3af">已吊销</span>'
            : `<button data-revoke="${escapeHtml(k.hash)}">吊销</button>`
        }</td>
      </tr>`;
    })
    .join("");

  rows.querySelectorAll("[data-revoke]").forEach((b) =>
    b.addEventListener("click", () => run(() => revoke(b.getAttribute("data-revoke")))),
  );
}

function formatTime(s) {
  const t = new Date(s);
  return Number.isNaN(t.getTime()) ? s || "" : t.toLocaleString("zh-CN");
}

async function onCreate(e) {
  e.preventDefault();
  const form = e.target;
  const user_id = form.user_id.value.trim();
  if (!user_id) return;

  const roles = form.roles.value
    .split(",")
    .map((r) => r.trim())
    .filter(Boolean);

  await run(async () => {
    const minted = await api("/keys", {
      method: "POST",
      body: JSON.stringify({
        user_id,
        name: form.name.value.trim(),
        label: form.label.value.trim(),
        roles,
      }),
    });

    // Shown once, on purpose: Core stores only the hash, so this value cannot
    // be recovered. Losing it means revoking and minting another.
    root.querySelector("#plain").textContent = minted.key;
    root.querySelector("#minted").style.display = "block";
    form.reset();
    await load();
  });
}

async function onCopy() {
  const text = root.querySelector("#plain").textContent;
  if (!text) return;
  const btn = root.querySelector("#copy");
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = "已复制";
  } catch {
    // Clipboard access can be denied; selecting the text is a usable fallback.
    const range = document.createRange();
    range.selectNodeContents(root.querySelector("#plain"));
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    btn.textContent = "已选中，按 ⌘C";
  }
  setTimeout(() => (btn.textContent = "复制"), 2000);
}

async function revoke(hash) {
  await api(`/keys/${encodeURIComponent(hash)}`, { method: "DELETE" });
  await load();
}

async function load() {
  const data = await api("/keys");
  state.keys = data.keys || [];
  renderList();
}

async function run(fn) {
  const err = root.querySelector("#err");
  try {
    await fn();
    if (err) err.style.display = "none";
  } catch (e) {
    if (!err) return;
    err.textContent =
      e.status === 403
        ? "需要管理员权限才能管理 API 密钥。"
        : e.status === 401
          ? "登录已失效，请重新登录。"
          : e.message || String(e);
    err.style.display = "block";
  }
}

function start(container) {
  const host = container || document;
  root = host.querySelector("#apikey-app");
  if (!root) return;
  renderShell();
  run(load);
}

renderWithQiankun({
  bootstrap() {},
  mount(props) {
    start(props.container);
  },
  unmount(props) {
    // Clearing this also drops any plaintext key still on screen, which is the
    // right thing to do with it.
    const el = (props.container || document).querySelector("#apikey-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
