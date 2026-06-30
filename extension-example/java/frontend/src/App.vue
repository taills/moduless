<script setup>
import { ref, onMounted } from "vue";

const API = "/api/extensions/java_example";
const items = ref([]);
const form = ref({ name: "", code: "", status: "active" });
const editing = ref(null);
const filter = ref("");
const error = ref("");

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

async function refresh() {
  try {
    const q = filter.value ? `?status=${encodeURIComponent(filter.value)}` : "";
    const data = await api("/items" + q);
    items.value = data.items || [];
    error.value = "";
  } catch (e) {
    error.value = e.message;
  }
}

async function submit() {
  try {
    const body = JSON.stringify(form.value);
    if (editing.value) {
      await api(`/items/${editing.value}`, { method: "PUT", body });
    } else {
      await api("/items", { method: "POST", body });
    }
    reset();
    await refresh();
  } catch (e) {
    error.value = e.message;
  }
}

function edit(it) {
  editing.value = it.id;
  form.value = { name: it.name, code: it.code, status: it.status };
}

function reset() {
  editing.value = null;
  form.value = { name: "", code: "", status: "active" };
}

async function remove(id) {
  try {
    await api(`/items/${id}`, { method: "DELETE" });
    await refresh();
  } catch (e) {
    error.value = e.message;
  }
}

onMounted(refresh);
</script>

<template>
  <div style="font-family: system-ui, sans-serif; max-width: 760px; margin: 0 auto; padding: 16px">
    <h2 style="margin: 0 0 4px">Java 扩展示例 · Items CRUD</h2>
    <p style="color: #666; margin: 0 0 16px">演示通过 Core 隧道访问 CMDS 的增删改查。</p>
    <div v-if="error" style="color: #c0392b; margin-bottom: 8px">{{ error }}</div>
    <form @submit.prevent="submit" style="display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 12px">
      <input v-model="form.name" placeholder="名称" required style="flex: 1; padding: 6px" />
      <input v-model="form.code" placeholder="编码(唯一)" required style="flex: 1; padding: 6px" />
      <select v-model="form.status" style="padding: 6px">
        <option value="active">active</option>
        <option value="inactive">inactive</option>
      </select>
      <button type="submit" style="padding: 6px 14px">{{ editing ? "更新" : "保存" }}</button>
      <button v-if="editing" type="button" style="padding: 6px 14px" @click="reset">取消</button>
    </form>
    <div style="margin-bottom: 8px">
      筛选状态：
      <select v-model="filter" @change="refresh" style="padding: 4px">
        <option value="">全部</option>
        <option value="active">active</option>
        <option value="inactive">inactive</option>
      </select>
    </div>
    <table style="width: 100%; border-collapse: collapse" border="1" cellpadding="6">
      <thead>
        <tr style="background: #f5f5f5">
          <th>名称</th><th>编码</th><th>状态</th><th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="items.length === 0">
          <td colspan="4" style="text-align: center; color: #999">暂无数据</td>
        </tr>
        <tr v-for="it in items" :key="it.id">
          <td>{{ it.name }}</td>
          <td>{{ it.code }}</td>
          <td>{{ it.status }}</td>
          <td>
            <button @click="edit(it)">编辑</button>
            <button @click="remove(it.id)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
