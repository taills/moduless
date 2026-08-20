// The notes plugin's console page.
//
// Plain DOM on purpose: an example plugin is something people copy, and a
// framework here would mean copying its build setup too. What is worth copying
// is the shape — the qiankun lifecycle at the bottom, the API wrapper that
// handles this codebase's two error formats, and escaping every value that
// came from a user.
//
// The api/escapeHtml helpers below are duplicated in every example plugin's
// frontend rather than shared from one place. That is deliberate: a plugin
// package has to be copyable on its own, and `../../shared/api.js` would break
// the moment someone lifts this directory out as a template.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/notes";

// Errors arrive in two shapes. This plugin returns {"error": "..."} but Core
// itself answers 401/404/503 with plain text, and so do several other plugins.
// Reading the body as JSON unconditionally turns "unauthenticated" into a
// parse error, which is the wrong thing to show someone.
async function api(path, options = {}) {
  const res = await fetch(API + path, {
    headers: { "Content-Type": "application/json" },
    // Same-origin, so Core's moduless_token cookie rides along on its own.
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
        // Content-Type claimed JSON and it was not. Keep the raw body.
      }
    }
    throw new Error(message);
  }

  return res.status === 204 ? null : res.json();
}

function escapeHtml(s) {
  return String(s ?? "").replace(
    /[&<>"']/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c],
  );
}

const state = { notes: [], next: "", total: 0, author: "" };
let root = null;

function query(reset) {
  const params = new URLSearchParams();
  if (state.author) params.set("author", state.author);
  if (!reset && state.next) params.set("after", state.next);
  const qs = params.toString();
  return qs ? `/notes?${qs}` : "/notes";
}

async function load(reset = true) {
  const data = await api(query(reset));
  state.notes = reset ? data.notes || [] : state.notes.concat(data.notes || []);
  state.next = data.next || "";
  renderList();
}

async function loadStats() {
  const { total } = await api("/stats");
  state.total = total;
  const el = root.querySelector("#total");
  if (el) el.textContent = String(total);
}

function renderShell() {
  root.innerHTML = `
    <style>
      .notes-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
      .notes-page h2 { margin: 0 0 4px; font-size: 22px; }
      .notes-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
      .notes-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
        border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; display: none; }
      .notes-page form { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 16px; }
      .notes-page input { padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 6px; font-size: 14px; }
      .notes-page input[name="title"] { flex: 0 0 220px; }
      .notes-page input[name="body"] { flex: 1; min-width: 200px; }
      .notes-page button { padding: 8px 14px; border: 1px solid #d1d5db; background: #fff;
        border-radius: 6px; cursor: pointer; font-size: 14px; }
      .notes-page button.primary { background: #2563eb; border-color: #2563eb; color: #fff; }
      .notes-page table { width: 100%; border-collapse: collapse; font-size: 14px; }
      .notes-page th, .notes-page td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #e5e7eb; }
      .notes-page th { color: #6b7280; font-weight: 500; font-size: 13px; }
      .notes-page .empty { text-align: center; color: #9ca3af; padding: 24px 0; }
      .notes-page .bar { display: flex; align-items: center; gap: 12px; margin-bottom: 12px; }
      .notes-page .count { color: #6b7280; font-size: 13px; margin-left: auto; }
    </style>
    <div class="notes-page">
      <h2>笔记</h2>
      <p class="sub">插件自己的表、自己的 API、自己的页面。数据存在 Core provision 的 ext_notes_notes 表里。</p>
      <div class="err" id="err"></div>
      <form id="create">
        <input name="title" placeholder="标题（必填）" required maxlength="200" />
        <input name="body" placeholder="内容" maxlength="2000" />
        <button type="submit" class="primary">新建</button>
      </form>
      <div class="bar">
        <input id="author" placeholder="按作者筛选" value="${escapeHtml(state.author)}" />
        <button id="apply">筛选</button>
        <span class="count">共 <b id="total">-</b> 条</span>
      </div>
      <table>
        <thead><tr><th>标题</th><th>内容</th><th>作者</th><th>创建时间</th><th></th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
      <div style="margin-top:12px"><button id="more" style="display:none">加载更多</button></div>
    </div>`;

  root.querySelector("#create").addEventListener("submit", onCreate);
  root.querySelector("#apply").addEventListener("click", onFilter);
  root.querySelector("#more").addEventListener("click", () => run(() => load(false)));
}

function renderList() {
  const rows = root.querySelector("#rows");
  if (!rows) return;

  if (state.notes.length === 0) {
    rows.innerHTML = `<tr><td colspan="5" class="empty">还没有笔记</td></tr>`;
  } else {
    rows.innerHTML = state.notes
      .map(
        (n) => `<tr>
          <td>${escapeHtml(n.title)}</td>
          <td>${escapeHtml(n.body)}</td>
          <td>${escapeHtml(n.author) || '<span style="color:#9ca3af">匿名</span>'}</td>
          <td>${escapeHtml(formatTime(n.created))}</td>
          <td><button data-del="${escapeHtml(n.id)}">删除</button></td>
        </tr>`,
      )
      .join("");
    rows.querySelectorAll("[data-del]").forEach((b) =>
      b.addEventListener("click", () => run(() => remove(b.getAttribute("data-del")))),
    );
  }

  const more = root.querySelector("#more");
  if (more) more.style.display = state.next ? "" : "none";
}

// Times come back as RFC3339 strings. The plugin's README explains why they are
// stored that way: comparisons are lexical, so the string order has to match
// time order for cursor paging to work.
function formatTime(s) {
  const t = new Date(s);
  return Number.isNaN(t.getTime()) ? s || "" : t.toLocaleString("zh-CN");
}

async function onCreate(e) {
  e.preventDefault();
  const form = e.target;
  const title = form.title.value.trim();
  if (!title) return;

  await run(async () => {
    await api("/notes", {
      method: "POST",
      body: JSON.stringify({ title, body: form.body.value.trim() }),
    });
    form.reset();
    state.next = "";
    await load(true);
    await loadStats();
  });
}

function onFilter() {
  state.author = root.querySelector("#author").value.trim();
  state.next = "";
  run(() => load(true));
}

async function remove(id) {
  await api(`/notes/${encodeURIComponent(id)}`, { method: "DELETE" });
  state.next = "";
  await load(true);
  await loadStats();
}

// One place that shows a failure, so no caller has to remember to.
async function run(fn) {
  const err = root.querySelector("#err");
  try {
    await fn();
    if (err) err.style.display = "none";
  } catch (e) {
    if (err) {
      err.textContent = e.message || String(e);
      err.style.display = "block";
    }
  }
}

function start(container) {
  const host = container || document;
  root = host.querySelector("#notes-app");
  if (!root) return;
  renderShell();
  run(async () => {
    await load(true);
    await loadStats();
  });
}

renderWithQiankun({
  bootstrap() {},
  mount(props) {
    start(props.container);
  },
  unmount(props) {
    // The host reuses the container across plugins, so leaving markup behind
    // would show up underneath whatever mounts next.
    const el = (props.container || document).querySelector("#notes-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

// Opened directly rather than through the console.
if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
