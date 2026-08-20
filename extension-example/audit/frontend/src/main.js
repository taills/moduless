// The audit plugin's console page.
//
// Read-only by nature: entries are written by a log-phase filter watching other
// plugins' requests, never through this page. What it needs to be good at is
// finding one thing in a lot of rows, so: filter by user, cursor paging.
//
// Plain DOM and a locally duplicated api()/escapeHtml are deliberate; see
// notes/frontend/src/main.js for why an example plugin does not share them.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/audit";

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

const state = { entries: [], next: "", user: "", limit: 50 };
let root = null;

function statusColor(status) {
  if (status >= 500) return "#b91c1c";
  if (status >= 400) return "#b45309";
  return "#15803d";
}

function renderShell() {
  root.innerHTML = `
    <style>
      .audit-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
      .audit-page h2 { margin: 0 0 4px; font-size: 22px; }
      .audit-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
      .audit-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .audit-page .bar { display: flex; gap: 8px; align-items: center; margin-bottom: 14px; }
      .audit-page input, .audit-page select { padding: 8px 10px; border: 1px solid #d1d5db;
        border-radius: 6px; font-size: 14px; }
      .audit-page button { padding: 8px 14px; border: 1px solid #d1d5db; background: #fff;
        border-radius: 6px; cursor: pointer; font-size: 14px; }
      .audit-page table { width: 100%; border-collapse: collapse; font-size: 14px; }
      .audit-page th, .audit-page td { text-align: left; padding: 9px 8px; border-bottom: 1px solid #e5e7eb; }
      .audit-page th { color: #6b7280; font-weight: 500; font-size: 13px; }
      .audit-page td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
      .audit-page .method { display: inline-block; min-width: 52px; font-weight: 600; font-size: 12px; }
      .audit-page .empty { text-align: center; color: #9ca3af; padding: 24px 0; }
    </style>
    <div class="audit-page">
      <h2>审计日志</h2>
      <p class="sub">由 log 阶段的 filter 记录，覆盖网关上的所有写请求 —— 不只是这个插件自己的路由。</p>
      <div class="err" id="err"></div>
      <div class="bar">
        <input id="user" placeholder="按用户筛选" value="${escapeHtml(state.user)}" />
        <select id="limit">
          <option value="50">每页 50</option>
          <option value="100">每页 100</option>
          <option value="200">每页 200</option>
        </select>
        <button id="apply">查询</button>
      </div>
      <table>
        <thead><tr><th>时间</th><th>用户</th><th>方法</th><th>路径</th><th>状态</th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
      <div style="margin-top:12px"><button id="more" style="display:none">加载更多</button></div>
    </div>`;

  root.querySelector("#limit").value = String(state.limit);
  root.querySelector("#apply").addEventListener("click", () => {
    state.user = root.querySelector("#user").value.trim();
    state.limit = Number(root.querySelector("#limit").value) || 50;
    state.next = "";
    run(() => load(true));
  });
  root.querySelector("#more").addEventListener("click", () => run(() => load(false)));
}

function renderList() {
  const rows = root.querySelector("#rows");
  if (!rows) return;

  if (state.entries.length === 0) {
    rows.innerHTML = `<tr><td colspan="5" class="empty">没有匹配的记录</td></tr>`;
  } else {
    rows.innerHTML = state.entries
      .map(
        (e) => `<tr>
          <td class="mono">${escapeHtml(formatTime(e.at))}</td>
          <td>${escapeHtml(e.user) || '<span style="color:#9ca3af">匿名</span>'}</td>
          <td><span class="method">${escapeHtml(e.method)}</span></td>
          <td class="mono">${escapeHtml(e.path)}</td>
          <td style="color:${statusColor(e.status)};font-weight:600">${escapeHtml(e.status)}</td>
        </tr>`,
      )
      .join("");
  }

  const more = root.querySelector("#more");
  if (more) more.style.display = state.next ? "" : "none";
}

function formatTime(s) {
  const t = new Date(s);
  return Number.isNaN(t.getTime()) ? s || "" : t.toLocaleString("zh-CN");
}

// Cursor paging, not offset: the plugin's own README explains that offset makes
// the database walk and discard rows, and shifts under concurrent writes.
async function load(reset = true) {
  const params = new URLSearchParams();
  if (state.user) params.set("user", state.user);
  params.set("limit", String(state.limit));
  if (!reset && state.next) params.set("cursor", state.next);

  const data = await api(`/entries?${params}`);
  state.entries = reset ? data.entries || [] : state.entries.concat(data.entries || []);
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
    err.textContent =
      e.status === 403
        ? "需要管理员权限才能查看审计日志。"
        : e.status === 401
          ? "登录已失效，请重新登录。"
          : e.message || String(e);
    err.style.display = "block";
  }
}

function start(container) {
  const host = container || document;
  root = host.querySelector("#audit-app");
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
    const el = (props.container || document).querySelector("#audit-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
