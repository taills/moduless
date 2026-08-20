// The redact plugin's console page.
//
// This plugin is a filter, not a destination: it rewrites other plugins'
// responses on the way out. So the page is not a CRUD screen, it answers one
// question — what is being redacted right now, and with what.
//
// Plain DOM and a locally duplicated api()/escapeHtml are deliberate; see
// notes/frontend/src/main.js for why an example plugin does not share them.
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/plugins/redact";

// Two error shapes: this codebase returns {"error": "..."} from some routes and
// bare text from others (including Core's own 401/403/503). The status is kept
// on the error so callers can tell "not allowed" from "went wrong".
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

let root = null;

const STYLE = `
  <style>
    .redact-page { font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: #1f2329; }
    .redact-page h2 { margin: 0 0 4px; font-size: 22px; }
    .redact-page .sub { color: #6b7280; margin: 0 0 20px; font-size: 14px; }
    .redact-page .err { color: #b91c1c; background: #fef2f2; border: 1px solid #fecaca;
      border-radius: 6px; padding: 10px 12px; margin-bottom: 12px; }
    .redact-page .card { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px;
      padding: 16px 18px; margin-bottom: 12px; }
    .redact-page .label { color: #6b7280; font-size: 13px; margin-bottom: 8px; }
    .redact-page .field { display: inline-block; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
      font-size: 13px; background: #f3f4f6; border: 1px solid #e5e7eb; border-radius: 4px;
      padding: 3px 8px; margin: 0 6px 6px 0; }
    .redact-page .mask { font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
      font-size: 14px; color: #b45309; }
    .redact-page .muted { color: #9ca3af; }
    .redact-page code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
  </style>`;

function renderError(e) {
  const message =
    e.status === 403
      ? "需要管理员权限才能查看脱敏配置。"
      : e.status === 401
        ? "登录已失效，请重新登录。"
        : e.message || String(e);

  root.innerHTML = `${STYLE}
    <div class="redact-page">
      <h2>响应脱敏</h2>
      <div class="err">${escapeHtml(message)}</div>
    </div>`;
}

function renderSettings(s) {
  const fields = s.fields || [];
  root.innerHTML = `${STYLE}
    <div class="redact-page">
      <h2>响应脱敏</h2>
      <p class="sub">
        这个插件是一个 post_handler filter，不提供自己的数据页面。它按字段名改写别的插件的
        JSON 响应 —— 下面是此刻生效的配置。改这些值请到「插件管理 → redact → 配置」。
      </p>
      <div class="card">
        <div class="label">被脱敏的字段（${fields.length}）</div>
        ${
          fields.length
            ? fields.map((f) => `<span class="field">${escapeHtml(f)}</span>`).join("")
            : '<span class="muted">没有配置任何字段，filter 不会改写任何响应。</span>'
        }
      </div>
      <div class="card">
        <div class="label">替换为</div>
        <span class="mask">${escapeHtml(s.mask)}</span>
      </div>
      <p class="sub">
        作用范围在 manifest 里写死为 <code>/api/plugins/notes/**</code> 和
        <code>/api/plugins/inventory/**</code>，不是 <code>/**</code> —— 因为
        needs_response_body 的 filter 会把每个响应体多跨进程搬运两次。
      </p>
    </div>`;
}

async function start(container) {
  const host = container || document;
  root = host.querySelector("#redact-app");
  if (!root) return;

  try {
    renderSettings(await api("/settings"));
  } catch (e) {
    renderError(e);
  }
}

renderWithQiankun({
  bootstrap() {},
  mount(props) {
    start(props.container);
  },
  unmount(props) {
    const el = (props.container || document).querySelector("#redact-app");
    if (el) el.innerHTML = "";
    root = null;
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  start();
}
