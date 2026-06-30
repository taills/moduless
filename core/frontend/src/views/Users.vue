<script setup>
// User management (admin). Lists system users and supports create, role change,
// password reset and deletion via /api/system/users.
import { ref, onMounted } from "vue";
import { api } from "../api";

const users = ref([]);
const error = ref("");
const loading = ref(false);

const showCreate = ref(false);
const form = ref({ username: "", password: "", role: "user" });

async function load() {
  error.value = "";
  loading.value = true;
  try {
    users.value = await api("/system/users");
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

async function create() {
  error.value = "";
  try {
    await api("/system/users", { method: "POST", body: JSON.stringify(form.value) });
    showCreate.value = false;
    form.value = { username: "", password: "", role: "user" };
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function changeRole(u) {
  const role = u.role === "admin" ? "user" : "admin";
  if (!confirm(`将 ${u.username} 的角色改为 ${role}？`)) return;
  try {
    await api(`/system/users/${u.id}`, { method: "PUT", body: JSON.stringify({ role }) });
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function resetPassword(u) {
  const password = prompt(`为 ${u.username} 设置新密码：`);
  if (!password) return;
  try {
    await api(`/system/users/${u.id}`, { method: "PUT", body: JSON.stringify({ password }) });
    alert("密码已重置");
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(u) {
  if (!confirm(`删除用户 ${u.username}？`)) return;
  try {
    await api(`/system/users/${u.id}`, { method: "DELETE" });
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

onMounted(load);
</script>

<template>
  <div class="panel">
    <div class="head">
      <h2>用户管理</h2>
      <button class="primary" @click="showCreate = !showCreate">新建用户</button>
    </div>
    <div v-if="error" class="err">{{ error }}</div>

    <form v-if="showCreate" class="create" @submit.prevent="create">
      <input v-model="form.username" placeholder="用户名" required />
      <input v-model="form.password" type="password" placeholder="密码" required />
      <select v-model="form.role">
        <option value="user">user</option>
        <option value="admin">admin</option>
      </select>
      <button class="primary" type="submit">创建</button>
    </form>

    <table>
      <thead>
        <tr><th>ID</th><th>用户名</th><th>角色</th><th>创建时间</th><th>操作</th></tr>
      </thead>
      <tbody>
        <tr v-for="u in users" :key="u.id">
          <td>{{ u.id }}</td>
          <td>{{ u.username }}</td>
          <td><span class="badge" :class="u.role">{{ u.role }}</span></td>
          <td>{{ u.created_at ? new Date(u.created_at).toLocaleString() : "-" }}</td>
          <td class="actions">
            <button @click="changeRole(u)">{{ u.role === "admin" ? "降为 user" : "升为 admin" }}</button>
            <button @click="resetPassword(u)">重置密码</button>
            <button class="danger" @click="remove(u)">删除</button>
          </td>
        </tr>
        <tr v-if="!loading && users.length === 0"><td colspan="5" class="empty">暂无用户</td></tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.panel { background: #fff; border: 1px solid #e5e7eb; border-radius: 10px; padding: 20px; }
.head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.head h2 { margin: 0; }
.err { color: #dc2626; margin-bottom: 12px; }
.create { display: flex; gap: 8px; margin-bottom: 14px; flex-wrap: wrap; }
.create input, .create select { padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 6px; }
table { width: 100%; border-collapse: collapse; font-size: 14px; }
th, td { text-align: left; padding: 10px 8px; border-bottom: 1px solid #f1f5f9; }
th { color: #6b7280; font-weight: 600; font-size: 12px; text-transform: uppercase; }
.actions { display: flex; gap: 6px; }
button { padding: 6px 10px; border: 1px solid #d1d5db; background: #fff; border-radius: 6px; cursor: pointer; font-size: 13px; }
button.primary { background: #2563eb; color: #fff; border-color: #2563eb; }
button.danger { color: #dc2626; border-color: #fecaca; }
.badge { padding: 2px 8px; border-radius: 999px; font-size: 12px; background: #e5e7eb; }
.badge.admin { background: #dbeafe; color: #1d4ed8; }
.empty { color: #9ca3af; text-align: center; padding: 16px; }
</style>
