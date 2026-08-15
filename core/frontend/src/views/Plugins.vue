<script setup>
import { onMounted, ref } from "vue";
import { api } from "../api";
import { refresh as refreshRegistry } from "../pluginRegistry";

const plugins = ref([]);
const error = ref("");
const busy = ref("");

async function load() {
  try {
    const data = await api("/system/plugins");
    plugins.value = data.plugins || [];
    error.value = "";
  } catch (e) {
    error.value = e.message;
  }
}

// Enable, disable and upgrade all start or stop operating-system processes, so
// they can take a second or two. The button is disabled while one is running
// to keep an impatient double-click from launching two enables at once.
async function act(key, action) {
  busy.value = key + ":" + action;
  try {
    const data = await api(`/system/plugins/${key}/${action}`, { method: "POST" });
    plugins.value = data.plugins || [];
    error.value = "";
    // Core pushes this over SSE too; refreshing here just makes the menu
    // update feel instant to whoever clicked.
    await refreshRegistry();
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = "";
  }
}

async function rescan() {
  busy.value = "*:rescan";
  try {
    const data = await api("/system/plugins/rescan", { method: "POST" });
    plugins.value = data.plugins || [];
    error.value = "";
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = "";
  }
}

function statusText(p) {
  if (p.load_error) return "加载失败";
  if (!p.enabled) return "已停用";
  if (p.ready === 0) return "启动中";
  if (p.ready < p.replicas) return `部分就绪 ${p.ready}/${p.replicas}`;
  return "运行中";
}

function statusClass(p) {
  if (p.load_error) return "bad";
  if (!p.enabled) return "off";
  if (p.ready === 0 || p.ready < p.replicas) return "warn";
  return "ok";
}

onMounted(load);
</script>

<template>
  <section>
    <header class="head">
      <h2>插件管理</h2>
      <button :disabled="busy" @click="rescan">重新扫描插件目录</button>
    </header>

    <p v-if="error" class="err">{{ error }}</p>

    <table v-if="plugins.length">
      <thead>
        <tr>
          <th>标识</th>
          <th>名称</th>
          <th>版本</th>
          <th>状态</th>
          <th>副本</th>
          <th>过滤器</th>
          <th>权限</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="p in plugins" :key="p.key">
          <td><code>{{ p.key }}</code></td>
          <td>{{ p.display_name || "-" }}</td>
          <td>{{ p.version || "-" }}</td>
          <td>
            <span class="badge" :class="statusClass(p)">{{ statusText(p) }}</span>
            <div v-if="p.load_error" class="load-error">{{ p.load_error }}</div>
          </td>
          <td>{{ p.ready }}/{{ p.replicas }}<span v-if="p.in_flight"> · 处理中 {{ p.in_flight }}</span></td>
          <td>{{ p.filters || 0 }}</td>
          <td class="perms">
            <span v-for="perm in p.permissions || []" :key="perm" class="perm">{{ perm }}</span>
            <span v-if="!(p.permissions || []).length" class="muted">无</span>
          </td>
          <td class="actions">
            <button v-if="!p.enabled" :disabled="!!busy || !!p.load_error" @click="act(p.key, 'enable')">启用</button>
            <button v-else :disabled="!!busy" @click="act(p.key, 'disable')">停用</button>
            <button :disabled="!!busy || !!p.load_error" @click="act(p.key, 'upgrade')">重载</button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="muted">插件目录为空。把插件包放进 Core 的 PLUGIN_DIR 后点击「重新扫描插件目录」。</p>
  </section>
</template>

<style scoped>
.head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
table {
  width: 100%;
  border-collapse: collapse;
  margin-top: 12px;
}
th,
td {
  text-align: left;
  padding: 10px 8px;
  border-bottom: 1px solid #e5e7eb;
  vertical-align: top;
}
th {
  font-weight: 600;
  color: #374151;
}
code {
  background: #f3f4f6;
  padding: 2px 6px;
  border-radius: 4px;
}
.badge {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 999px;
  font-size: 12px;
}
.badge.ok {
  background: #dcfce7;
  color: #166534;
}
.badge.warn {
  background: #fef9c3;
  color: #854d0e;
}
.badge.off {
  background: #e5e7eb;
  color: #4b5563;
}
.badge.bad {
  background: #fee2e2;
  color: #991b1b;
}
.load-error {
  margin-top: 4px;
  font-size: 12px;
  color: #991b1b;
  max-width: 320px;
}
.perms {
  max-width: 220px;
}
.perm {
  display: inline-block;
  background: #eef2ff;
  color: #3730a3;
  border-radius: 4px;
  padding: 1px 6px;
  margin: 0 4px 4px 0;
  font-size: 12px;
}
.actions button {
  margin-right: 6px;
}
.muted {
  color: #6b7280;
}
.err {
  color: #b91c1c;
}
</style>
