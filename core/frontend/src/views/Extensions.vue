<script setup>
// Extension management (admin). Shows the registry with approval status
// (待注册 / 已注册 / 已拒绝) and live runtime state, and drives the approval
// workflow plus per-key secret management via /api/system/extensions.
import { ref, onMounted } from "vue";
import { api } from "../api";

const items = ref([]);
const error = ref("");
const loading = ref(false);

// Per-key expanded secret panel state.
const openKey = ref("");
const secrets = ref([]);

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
    if (openKey.value === ext.key) openKey.value = "";
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function toggleSecrets(ext) {
  if (openKey.value === ext.key) {
    openKey.value = "";
    return;
  }
  openKey.value = ext.key;
  await loadSecrets(ext.key);
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

onMounted(load);
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
        <tr><th>扩展</th><th>状态</th><th>在线</th><th>副本</th><th>待审实例</th><th>操作</th></tr>
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
            <td>{{ ext.replicas }}</td>
            <td>{{ ext.pending_instances }}</td>
            <td class="actions">
              <button v-if="ext.status !== 'approved'" class="primary" @click="approve(ext)">批准</button>
              <button v-if="ext.status !== 'rejected'" @click="reject(ext)">拒绝</button>
              <button v-if="ext.status === 'approved'" @click="toggleSecrets(ext)">密钥</button>
              <button class="danger" @click="remove(ext)">删除</button>
            </td>
          </tr>
          <tr v-if="openKey === ext.key" class="secrets-row">
            <td colspan="6">
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
        <tr v-if="!loading && items.length === 0"><td colspan="6" class="empty">暂无扩展注册</td></tr>
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
.actions { display: flex; gap: 6px; flex-wrap: wrap; }
button { padding: 6px 10px; border: 1px solid #d1d5db; background: #fff; border-radius: 6px; cursor: pointer; font-size: 13px; }
button.primary { background: #2563eb; color: #fff; border-color: #2563eb; }
button.danger { color: #dc2626; border-color: #fecaca; }
.badge { padding: 2px 8px; border-radius: 999px; font-size: 12px; background: #e5e7eb; }
.badge.pending { background: #fef3c7; color: #b45309; }
.badge.approved { background: #dcfce7; color: #15803d; }
.badge.rejected { background: #fee2e2; color: #b91c1c; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #9ca3af; margin-right: 6px; }
.dot.on { background: #22c55e; }
.secrets-row td { background: #f9fafb; }
.secrets-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
table.inner { background: #fff; border: 1px solid #e5e7eb; border-radius: 6px; }
.empty { color: #9ca3af; text-align: center; padding: 16px; }
</style>
