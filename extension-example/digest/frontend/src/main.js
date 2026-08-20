// The digest plugin's console page.
//
// Snapshots are produced by a cron job, never by this page, so the only actions
// are looking and downloading. The download is the interesting part: the plugin
// mints a short-lived URL and the browser fetches the bytes from Core directly,
// because binary content deliberately does not travel over the plugin transport.
//
// Plain DOM and a locally duplicated api()/escapeHtml are deliberate; see
// notes/frontend/src/main.js for why an example plugin does not share them.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/digest";

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

const state = { snapshots: [] };
let root = null;

function renderShell() {
  root.innerHTML = `
    <style>
      .digest-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
      .digest-page h2 { margin: 0 0 4px; font-size: 22px; }
      .digest-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
      .digest-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .digest-page .ok { color: #15803d; background: #f0fdf4; border: 1px solid #bbf7d0;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .digest-page table { width: 100%; border-collapse: collapse; font-size: 14px; }
      .digest-page th, .digest-page td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #e5e7eb; }
      .digest-page th { color: #6b7280; font-weight: 500; font-size: 13px; }
      .digest-page td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
      .digest-page button { padding: 6px 12px; border: 1px solid #d1d5db; background: #fff;
        border-radius: 6px; cursor: pointer; font-size: 13px; }
      .digest-page .empty { text-align: center; color: #9ca3af; padding: 24px 0; }
      .digest-page a { color: #2563eb; }
    </style>
    <div class="digest-page">
      <h2>外部快照</h2>
      <p class="sub">定时任务抓取外部源并存档。最新 50 条，新的在前。</p>
      <div class="err" id="err"></div>
      <div class="ok" id="ok"></div>
      <table>
        <thead><tr><th>时间</th><th>来源</th><th>大小</th><th>SHA-256</th><th></th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
    </div>`;
}

function renderList() {
  const rows = root.querySelector("#rows");
  if (!rows) return;

  if (state.snapshots.length === 0) {
    rows.innerHTML = `<tr><td colspan="5" class="empty">还没有快照，等定时任务跑一次</td></tr>`;
    return;
  }

  rows.innerHTML = state.snapshots
    .map(
      (s) => `<tr>
        <td>${escapeHtml(formatTime(s.taken_at))}</td>
        <td>${escapeHtml(s.source)}</td>
        <td>${escapeHtml(formatBytes(s.bytes))}</td>
        <td class="mono" title="${escapeHtml(s.sha256)}">${escapeHtml(String(s.sha256 || "").slice(0, 16))}…</td>
        <td><button data-id="${escapeHtml(s.id)}">下载</button></td>
      </tr>`,
    )
    .join("");

  rows.querySelectorAll("[data-id]").forEach((b) =>
    b.addEventListener("click", () => download(b.getAttribute("data-id"))),
  );
}

// taken_at is unix seconds, not RFC3339 — it comes from the job's scheduled time.
function formatTime(sec) {
  const t = new Date(Number(sec) * 1000);
  return Number.isNaN(t.getTime()) ? String(sec ?? "") : t.toLocaleString("zh-CN");
}

function formatBytes(n) {
  const b = Number(n) || 0;
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / 1024 / 1024).toFixed(1)} MB`;
}

// The plugin hands back a URL rather than the bytes. Opening it in a new tab
// lets Core serve the file directly, which is the whole point of the design.
async function download(id) {
  const err = root.querySelector("#err");
  const ok = root.querySelector("#ok");
  try {
    const { url, expires_in } = await api(`/snapshots/${encodeURIComponent(id)}/download`);
    err.style.display = "none";
    ok.textContent = `下载链接已生成（${expires_in} 后失效），正在打开…`;
    ok.style.display = "block";
    window.open(url, "_blank", "noopener");
  } catch (e) {
    ok.style.display = "none";
    err.textContent =
      e.status === 401 ? "需要登录后才能下载快照。" : e.message || String(e);
    err.style.display = "block";
  }
}

async function load() {
  const err = root.querySelector("#err");
  try {
    const data = await api("/snapshots");
    state.snapshots = data.snapshots || [];
    renderList();
    err.style.display = "none";
  } catch (e) {
    err.textContent = e.message || String(e);
    err.style.display = "block";
  }
}

function start(container) {
  const host = container || document;
  root = host.querySelector("#digest-app");
  if (!root) return;
  renderShell();
  load();
}

renderWithQiankun({
  bootstrap() {},
  mount(props) {
    start(props.container);
  },
  unmount(props) {
    const el = (props.container || document).querySelector("#digest-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
