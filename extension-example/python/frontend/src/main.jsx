import React from "react";
import { createRoot } from "react-dom/client";
import { renderWithQiankun, qiankunWindow } from "vite-plugin-qiankun/dist/helper";

const API = "/api/extensions/python_example";

async function api(path, options = {}) {
  const res = await fetch(API + path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ detail: res.statusText }));
    throw new Error(err.detail || err.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}

const EMPTY = { name: "", code: "", status: "active" };

function App() {
  const [items, setItems] = React.useState([]);
  const [form, setForm] = React.useState(EMPTY);
  const [editing, setEditing] = React.useState(null);
  const [filter, setFilter] = React.useState("");
  const [error, setError] = React.useState("");

  const refresh = React.useCallback(async () => {
    try {
      const q = filter ? `?status=${encodeURIComponent(filter)}` : "";
      const data = await api("/items" + q);
      setItems(data.items || []);
      setError("");
    } catch (e) {
      setError(e.message);
    }
  }, [filter]);

  React.useEffect(() => {
    refresh();
  }, [refresh]);

  async function submit(e) {
    e.preventDefault();
    try {
      const body = JSON.stringify(form);
      if (editing) {
        await api(`/items/${editing}`, { method: "PUT", body });
      } else {
        await api("/items", { method: "POST", body });
      }
      setForm(EMPTY);
      setEditing(null);
      await refresh();
    } catch (e) {
      setError(e.message);
    }
  }

  async function remove(id) {
    try {
      await api(`/items/${id}`, { method: "DELETE" });
      await refresh();
    } catch (e) {
      setError(e.message);
    }
  }

  function edit(it) {
    setEditing(it.id);
    setForm({ name: it.name, code: it.code, status: it.status });
  }

  const cell = { border: "1px solid #ddd", padding: 6 };
  return (
    <div style={{ fontFamily: "system-ui, sans-serif", maxWidth: 760, margin: "0 auto", padding: 16 }}>
      <h2 style={{ margin: "0 0 4px" }}>Python 扩展示例 · Items CRUD</h2>
      <p style={{ color: "#666", margin: "0 0 16px" }}>演示通过 Core 隧道访问 CMDS 的增删改查。</p>
      {error && <div style={{ color: "#c0392b", marginBottom: 8 }}>{error}</div>}
      <form onSubmit={submit} style={{ display: "flex", gap: 8, flexWrap: "wrap", marginBottom: 12 }}>
        <input
          placeholder="名称" required style={{ flex: 1, padding: 6 }}
          value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })}
        />
        <input
          placeholder="编码(唯一)" required style={{ flex: 1, padding: 6 }}
          value={form.code} onChange={(e) => setForm({ ...form, code: e.target.value })}
        />
        <select value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value })} style={{ padding: 6 }}>
          <option value="active">active</option>
          <option value="inactive">inactive</option>
        </select>
        <button type="submit" style={{ padding: "6px 14px" }}>{editing ? "更新" : "保存"}</button>
        {editing && (
          <button type="button" style={{ padding: "6px 14px" }} onClick={() => { setEditing(null); setForm(EMPTY); }}>
            取消
          </button>
        )}
      </form>
      <div style={{ marginBottom: 8 }}>
        筛选状态：
        <select value={filter} onChange={(e) => setFilter(e.target.value)} style={{ padding: 4 }}>
          <option value="">全部</option>
          <option value="active">active</option>
          <option value="inactive">inactive</option>
        </select>
      </div>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ background: "#f5f5f5" }}>
            <th style={cell}>名称</th><th style={cell}>编码</th><th style={cell}>状态</th><th style={cell}>操作</th>
          </tr>
        </thead>
        <tbody>
          {items.length === 0 ? (
            <tr><td style={{ ...cell, textAlign: "center", color: "#999" }} colSpan={4}>暂无数据</td></tr>
          ) : (
            items.map((it) => (
              <tr key={it.id}>
                <td style={cell}>{it.name}</td>
                <td style={cell}>{it.code}</td>
                <td style={cell}>{it.status}</td>
                <td style={cell}>
                  <button onClick={() => edit(it)}>编辑</button>{" "}
                  <button onClick={() => remove(it.id)}>删除</button>
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

let root;

function render(container) {
  const el = container
    ? container.querySelector("#python-example-root")
    : document.getElementById("python-example-root");
  root = createRoot(el);
  root.render(<App />);
}

// Qiankun lifecycle (via vite-plugin-qiankun) so the host can mount this app.
renderWithQiankun({
  bootstrap() {},
  mount(props) {
    render(props.container);
  },
  unmount() {
    if (root) {
      root.unmount();
      root = null;
    }
  },
  update() {},
});

if (!qiankunWindow.__POWERED_BY_QIANKUN__) {
  render();
}
