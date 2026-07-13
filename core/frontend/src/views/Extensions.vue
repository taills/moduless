<script setup>
// Extension management (admin). Shows the registry with approval status
// (待注册 / 已注册 / 已拒绝) and live runtime state, and drives the approval
// workflow plus per-key secret management via /api/system/extensions.
import { ref, computed, onMounted, onUnmounted } from "vue";
import { api } from "../api";

const items = ref([]);
const error = ref("");
const loading = ref(false);

// Two independent panels per row: secrets and replicas. Storing both keys
// lets an admin have both open at once when comparing secrets vs. live state.
const openPanel = ref({ key: "", panel: "" }); // panel: "" | "secrets" | "replicas"
const secrets = ref([]);

// Auto-refresh every 15s while the tab is visible, so last_ping/weight reflect
// live tunnel state without the admin needing to refresh manually.
const POLL_MS = 15000;
let pollTimer = null;
let visibilityHandler = null;

const STATUS_LABEL = { pending: "待注册", approved: "已注册", rejected: "已拒绝" };

async function load() {
  error.value = "";
  loading.value = true;
  try {
    items.value = await api("/system/extensions");
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

function togglePanel(ext, panel) {
  if (openPanel.value.key === ext.key && openPanel.value.panel === panel) {
    openPanel.value = { key: "", panel: "" };
    return;
  }
  openPanel.value = { key: ext.key, panel };
  if (panel === "secrets") {
    loadSecrets(ext.key);
  }
}

// Format an absolute timestamp as "12:34:56" or "—" when missing.
function formatTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleTimeString();
}

// "12s 前" / "3m 前" / "—" for a relative-from-now display.
function formatAgo(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  const sec = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000));
  if (sec < 60) return `${sec}s 前`;
  if (sec < 3600) return `${Math.round(sec / 60)}m 前`;
  return d.toLocaleString();
}

const hasAnyReplicas = computed(() =>
  items.value.some((it) => Array.isArray(it.replicas_detail) && it.replicas_detail.length > 0)
);

async function approve(ext) {
  if (!confirm(`批准扩展 ${ext.key}？Core 将向其下发密钥并激活。`)) return;
  try {
    const res = await api(`/system/extensions/${ext.key}/approve`, { method: "POST" });
    const issued = (res.issued_secrets || []).map((s) => `${s.instance_id}: ${s.secret}`).join("\n");
    if (issued) {
      alert(`已批准。下发的密钥（已自动写入扩展 manifest，仅此一次可见）：\n\n${issued}`);
    } else {
      alert("已批准。当前无在线待审实例；可在密钥面板生成密钥供副本使用。");
    }
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function reject(ext) {
  if (!confirm(`拒绝扩展 ${ext.key}？将吊销其所有密钥并断开连接。`)) return;
  try {
    await api(`/system/extensions/${ext.key}/reject`, { method: "POST" });
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(ext) {
  if (!confirm(`删除扩展 ${ext.key}？删除后它可重新发起注册申请。`)) return;
  try {
    await api(`/system/extensions/${ext.key}`, { method: "DELETE" });
    if (openPanel.value.key === ext.key) openPanel.value = { key: "", panel: "" };
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function loadSecrets(key) {
  try {
    secrets.value = await api(`/system/extensions/${key}/secrets`);
  } catch (e) {
    error.value = e.message;
  }
}

async function generateSecret(key) {
  const label = prompt("为新密钥设置标签（如 replica-2）：", "");
  if (label === null) return;
  try {
    const res = await api(`/system/extensions/${key}/secrets`, {
      method: "POST",
      body: JSON.stringify({ label }),
    });
    alert(`新密钥（仅此一次可见，请烤入对应副本的 EXTENSION_SECRET）：\n\n${res.secret}`);
    await loadSecrets(key);
  } catch (e) {
    error.value = e.message;
  }
}

async function revokeSecret(key, id) {
  if (!confirm("吊销该密钥？使用它的实例将无法再接入。")) return;
  try {
    await api(`/system/extensions/${key}/secrets/${id}`, { method: "DELETE" });
    await loadSecrets(key);
  } catch (e) {
    error.value = e.message;
  }
}

onMounted(() => {
  load();
  pollTimer = setInterval(() => {
    if (document.visibilityState === "visible") load();
  }, POLL_MS);
  visibilityHandler = () => {
    if (document.visibilityState === "visible") load();
  };
  document.addEventListener("visibilitychange", visibilityHandler);
});

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer);
  if (visibilityHandler) document.removeEventListener("visibilitychange", visibilityHandler);
});
</script>

<template>
  <div class="panel">
    <div class="head">
      <h2>扩展管理</h2>
      <button @click="load">刷新</button>
    </div>
    <div v-if="error" class="err">{{ error }}</div>

    <table>
      <thead>
        <tr>
          <th>扩展</th>
          <th>状态</th>
          <th>在线</th>
          <th>最后通讯</th>
          <th>权重</th>
          <th>副本</th>
          <th>待审实例</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="ext in items" :key="ext.key">
          <tr>
            <td>
              <div class="name">{{ ext.display_name || ext.key }}</div>
              <div class="key">{{ ext.key }} · v{{ ext.version || "?" }}</div>
            </td>
            <td><span class="badge" :class="ext.status">{{ STATUS_LABEL[ext.status] || ext.status }}</span></td>
            <td><span class="dot" :class="{ on: ext.online }"></span>{{ ext.online ? "在线" : "离线" }}</td>
            <td>
              <div class="time">{{ formatTime(ext.last_ping) }}</div>
              <div class="ago">{{ formatAgo(ext.last_ping) }}</div>
            </td>
            <td>
              <div class="weight">{{ ext.weight }}</div>
              <div class="hint" v-if="ext.replicas">（{{ ext.replicas }} 副本）</div>
            </td>
            <td>
              <button class="link" @click="togglePanel(ext, 'replicas')">
                {{ ext.replicas || 0 }}
                <span class="caret">{{ openPanel.key === ext.key && openPanel.panel === "replicas" ? "▴" : "▾" }}</span>
              </button>
            </td>
            <td>
              <button v-if="ext.pending_instances > 0" class="link warn-link" @click="togglePanel(ext, 'pending')">
                {{ ext.pending_instances }}
                <span class="caret">{{ openPanel.key === ext.key && openPanel.panel === "pending" ? "▴" : "▾" }}</span>
              </button>
              <span v-else>0</span>
            </td>
            <td class="actions">
              <button v-if="ext.status !== 'approved' || ext.pending_instances > 0" class="primary" @click="approve(ext)">批准</button>
              <button v-if="ext.status !== 'rejected'" @click="reject(ext)">拒绝</button>
              <button v-if="ext.status === 'approved'" @click="togglePanel(ext, 'secrets')">密钥</button>
              <button class="danger" @click="remove(ext)">删除</button>
            </td>
          </tr>
          <tr v-if="openPanel.key === ext.key && openPanel.panel === 'replicas'" class="replicas-row">
            <td colspan="8">
              <div class="replicas">
                <div class="replicas-head">
                  <strong>已路由副本（携带密钥）</strong>
                </div>
                <table class="inner">
                  <thead><tr><th>实例 ID</th><th>权重</th><th>最后心跳</th><th>相对</th><th>状态</th></tr></thead>
                  <tbody>
                    <tr v-for="r in ext.replicas_detail || []" :key="r.instance_id">
                      <td><code>{{ r.instance_id }}</code></td>
                      <td>{{ r.weight }}</td>
                      <td>{{ formatTime(r.last_ping) }}</td>
                      <td>{{ formatAgo(r.last_ping) }}</td>
                      <td><span class="dot" :class="{ on: r.online, warn: !r.online }"></span>{{ r.online ? "在线" : "离线" }}</td>
                    </tr>
                    <tr v-if="!ext.replicas_detail || ext.replicas_detail.length === 0"><td colspan="5" class="empty">暂无副本</td></tr>
                  </tbody>
                </table>
              </div>
            </td>
          </tr>
          <tr v-if="openPanel.key === ext.key && openPanel.panel === 'pending'" class="pending-row">
            <td colspan="8">
              <div class="replicas">
                <div class="replicas-head">
                  <strong>待审实例（无密钥连接，等待批准后签发密钥并路由）</strong>
                  <button class="primary" @click="approve(ext)">批准全部</button>
                </div>
                <table class="inner">
                  <thead><tr><th>实例 ID</th><th>版本</th><th>模式</th><th>连接时间</th><th>相对</th><th>状态</th></tr></thead>
                  <tbody>
                    <tr v-for="p in ext.pending_detail || []" :key="p.instance_id">
                      <td><code>{{ p.instance_id }}</code></td>
                      <td>v{{ p.version || "?" }}</td>
                      <td>{{ p.is_dev ? "dev" : "prod" }}</td>
                      <td>{{ formatTime(p.connected_at) }}</td>
                      <td>{{ formatAgo(p.connected_at) }}</td>
                      <td><span class="badge pending">待审·无密钥</span></td>
                    </tr>
                    <tr v-if="!ext.pending_detail || ext.pending_detail.length === 0"><td colspan="6" class="empty">暂无待审实例</td></tr>
                  </tbody>
                </table>
              </div>
            </td>
          </tr>
          <tr v-if="openPanel.key === ext.key && openPanel.panel === 'secrets'" class="secrets-row">
            <td colspan="8">
              <div class="secrets">
                <div class="secrets-head">
                  <strong>密钥（一个 key 可有多个，供多实例使用）</strong>
                  <button class="primary" @click="generateSecret(ext.key)">生成新密钥</button>
                </div>
                <table class="inner">
                  <thead><tr><th>ID</th><th>标签</th><th>创建</th><th>最近使用</th><th>状态</th><th></th></tr></thead>
                  <tbody>
                    <tr v-for="s in secrets" :key="s.id">
                      <td>{{ s.id }}</td>
                      <td>{{ s.label || "-" }}</td>
                      <td>{{ s.created_at ? new Date(s.created_at).toLocaleString() : "-" }}</td>
                      <td>{{ s.last_used_at ? new Date(s.last_used_at).toLocaleString() : "-" }}</td>
                      <td>{{ s.revoked_at ? "已吊销" : "有效" }}</td>
                      <td><button v-if="!s.revoked_at" class="danger" @click="revokeSecret(ext.key, s.id)">吊销</button></td>
                    </tr>
                    <tr v-if="secrets.length === 0"><td colspan="6" class="empty">暂无密钥</td></tr>
                  </tbody>
                </table>
              </div>
            </td>
          </tr>
        </template>
        <tr v-if="!loading && items.length === 0"><td colspan="8" class="empty">暂无扩展注册</td></tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.panel { background: #fff; border: 1px solid #e5e7eb; border-radius: 10px; padding: 20px; }
.head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.head h2 { margin: 0; }
.err { color: #dc2626; margin-bottom: 12px; }
table { width: 100%; border-collapse: collapse; font-size: 14px; }
th, td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #f1f5f9; vertical-align: top; }
th { color: #6b7280; font-weight: 600; font-size: 12px; text-transform: uppercase; }
.name { font-weight: 600; }
.key { color: #9ca3af; font-size: 12px; margin-top: 2px; }
.time { font-variant-numeric: tabular-nums; }
.ago { color: #9ca3af; font-size: 12px; margin-top: 2px; }
.weight { font-weight: 600; }
.hint { color: #9ca3af; font-size: 12px; margin-top: 2px; }
.actions { display: flex; gap: 6px; flex-wrap: wrap; }
button { padding: 6px 10px; border: 1px solid #d1d5db; background: #fff; border-radius: 6px; cursor: pointer; font-size: 13px; }
button.primary { background: #2563eb; color: #fff; border-color: #2563eb; }
button.danger { color: #dc2626; border-color: #fecaca; }
button.link { background: transparent; border: none; padding: 2px 0; color: #2563eb; }
button.link:hover { text-decoration: underline; }
button.link.warn-link { color: #b45309; font-weight: 600; }
.caret { font-size: 11px; margin-left: 2px; color: #6b7280; }
.badge { padding: 2px 8px; border-radius: 999px; font-size: 12px; background: #e5e7eb; }
.badge.pending { background: #fef3c7; color: #b45309; }
.badge.approved { background: #dcfce7; color: #15803d; }
.badge.rejected { background: #fee2e2; color: #b91c1c; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #9ca3af; margin-right: 6px; }
.dot.on { background: #22c55e; }
.dot.warn { background: #f59e0b; }
.secrets-row td, .replicas-row td { background: #f9fafb; }
.pending-row td { background: #fffbeb; }
.secrets-head, .replicas-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
table.inner { background: #fff; border: 1px solid #e5e7eb; border-radius: 6px; }
code { background: #f3f4f6; padding: 1px 5px; border-radius: 3px; font-size: 12px; }
.empty { color: #9ca3af; text-align: center; padding: 16px; }
</style>
