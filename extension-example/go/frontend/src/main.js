// Vanilla micro-frontend for the Go extension example: a full CRUD UI for the
// "items" collection, served by Core and proxied to the Go backend over the
// gRPC tunnel.
const API = "/api/extensions/go_example";

async function api(path, options = {}) {
  const res = await fetch(API + path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}

const state = { items: [], editing: null, filter: "" };

async function refresh() {
  const q = state.filter ? `?status=${encodeURIComponent(state.filter)}` : "";
  const data = await api("/items" + q);
  state.items = data.items || [];
  renderList();
}

function renderApp(root) {
  root.innerHTML = `
    <div style="font-family:system-ui,sans-serif;max-width:760px;margin:0 auto;padding:16px">
      <h2 style="margin:0 0 4px">Go 扩展示例 · Items CRUD</h2>
      <p style="color:#666;margin:0 0 16px">演示通过 Core 隧道访问 CMDS 的增删改查。</p>
      <div id="err" style="color:#c0392b;margin-bottom:8px"></div>
      <form id="form" style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:12px">
        <input name="name" placeholder="名称" required style="flex:1;padding:6px" />
        <input name="code" placeholder="编码(唯一)" required style="flex:1;padding:6px" />
        <select name="status" style="padding:6px">
          <option value="active">active</option>
          <option value="inactive">inactive</option>
        </select>
        <button type="submit" style="padding:6px 14px">保存</button>
        <button type="button" id="reset" style="padding:6px 14px">清空</button>
      </form>
      <div style="margin-bottom:8px">
        筛选状态：
        <select id="filter" style="padding:4px">
          <option value="">全部</option>
          <option value="active">active</option>
          <option value="inactive">inactive</option>
        </select>
      </div>
      <table style="width:100%;border-collapse:collapse" border="1" cellpadding="6">
        <thead><tr style="background:#f5f5f5"><th>名称</th><th>编码</th><th>状态</th><th>操作</th></tr></thead>
        <tbody id="rows"></tbody>
      </table>
    </div>`;

  root.querySelector("#form").addEventListener("submit", onSubmit);
  root.querySelector("#reset").addEventListener("click", resetForm);
  root.querySelector("#filter").addEventListener("change", (e) => {
    state.filter = e.target.value;
    refresh().catch(showError);
  });
}

function renderList() {
  const rows = document.getElementById("rows");
  if (!rows) return;
  if (state.items.length === 0) {
    rows.innerHTML = `<tr><td colspan="4" style="text-align:center;color:#999">暂无数据</td></tr>`;
    return;
  }
  rows.innerHTML = state.items
    .map(
      (it) => `<tr>
        <td>${escapeHtml(it.name)}</td>
        <td>${escapeHtml(it.code)}</td>
        <td>${escapeHtml(it.status)}</td>
        <td>
          <button data-edit="${it.id}">编辑</button>
          <button data-del="${it.id}">删除</button>
        </td>
      </tr>`
    )
    .join("");
  rows.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => startEdit(b.getAttribute("data-edit")))
  );
  rows.querySelectorAll("[data-del]").forEach((b) =>
    b.addEventListener("click", () => removeItem(b.getAttribute("data-del")))
  );
}

function startEdit(id) {
  const item = state.items.find((i) => i.id === id);
  if (!item) return;
  state.editing = id;
  const form = document.getElementById("form");
  form.name.value = item.name;
  form.code.value = item.code;
  form.status.value = item.status;
}

function resetForm() {
  state.editing = null;
  document.getElementById("form").reset();
}

async function onSubmit(e) {
  e.preventDefault();
  const form = e.target;
  const body = JSON.stringify({
    name: form.name.value.trim(),
    code: form.code.value.trim(),
    status: form.status.value,
  });
  try {
    if (state.editing) {
      await api(`/items/${state.editing}`, { method: "PUT", body });
    } else {
      await api("/items", { method: "POST", body });
    }
    resetForm();
    await refresh();
  } catch (err) {
    showError(err);
  }
}

async function removeItem(id) {
  try {
    await api(`/items/${id}`, { method: "DELETE" });
    await refresh();
  } catch (err) {
    showError(err);
  }
}

function showError(err) {
  const el = document.getElementById("err");
  if (el) el.textContent = err.message || String(err);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])
  );
}

async function start() {
  const root = document.getElementById("app");
  renderApp(root);
  await refresh().catch(showError);
}

// Qiankun lifecycle hooks.
export async function bootstrap() {}
export async function mount() {
  await start();
}
export async function unmount() {
  const root = document.getElementById("app");
  if (root) root.innerHTML = "";
}

if (!window.__POWERED_BY_QIANKUN__) {
  start();
}
