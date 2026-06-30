<script setup>
import { ref } from "vue";
import { useRouter } from "vue-router";
import { api, auth } from "../api";

const router = useRouter();
const username = ref("admin");
const password = ref("");
const error = ref("");
const loading = ref(false);

async function submit() {
  error.value = "";
  loading.value = true;
  try {
    const data = await api("/system/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: username.value, password: password.value }),
    });
    auth.setToken(data.token);
    localStorage.setItem("moduless_user", JSON.stringify(data.user));
    router.push("/");
  } catch (e) {
    error.value = e.message === "unauthenticated" ? "用户名或密码错误" : e.message;
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <div class="login-wrap">
    <form class="login-card" @submit.prevent="submit">
      <h1>Moduless 控制台</h1>
      <p class="sub">登录以管理模块化扩展</p>
      <label>用户名<input v-model="username" autocomplete="username" required /></label>
      <label>密码<input v-model="password" type="password" autocomplete="current-password" required /></label>
      <div v-if="error" class="err">{{ error }}</div>
      <button type="submit" :disabled="loading">{{ loading ? "登录中…" : "登录" }}</button>
      <p class="hint">默认账号：admin / admin123（可用 ADMIN_PASSWORD 覆盖）</p>
    </form>
  </div>
</template>

<style scoped>
.login-wrap {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1e3a8a, #0ea5e9);
}
.login-card {
  width: 340px;
  background: #fff;
  border-radius: 12px;
  padding: 32px;
  box-shadow: 0 10px 40px rgba(0, 0, 0, 0.2);
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.login-card h1 {
  margin: 0;
  font-size: 22px;
}
.sub {
  margin: 0 0 8px;
  color: #6b7280;
  font-size: 14px;
}
.login-card label {
  display: flex;
  flex-direction: column;
  font-size: 13px;
  color: #374151;
  gap: 4px;
}
.login-card input {
  padding: 9px 10px;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  font-size: 14px;
}
.login-card button {
  margin-top: 6px;
  padding: 10px;
  background: #2563eb;
  color: #fff;
  border: none;
  border-radius: 6px;
  font-size: 15px;
  cursor: pointer;
}
.login-card button:disabled {
  opacity: 0.6;
  cursor: default;
}
.err {
  color: #dc2626;
  font-size: 13px;
}
.hint {
  margin: 4px 0 0;
  color: #9ca3af;
  font-size: 12px;
}
</style>
