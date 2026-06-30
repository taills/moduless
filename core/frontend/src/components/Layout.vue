<script setup>
import { ref, onMounted } from "vue";
import { useRouter, useRoute } from "vue-router";
import { api, auth } from "../api";
import { setupMicroApps } from "../microApps";

const router = useRouter();
const route = useRoute();
const apps = ref([]);
const error = ref("");
const user = ref(JSON.parse(localStorage.getItem("moduless_user") || "null"));

onMounted(async () => {
  try {
    // Registers qiankun micro-apps and returns the menu list.
    apps.value = await setupMicroApps();
  } catch (e) {
    error.value = e.message;
    if (e.message === "unauthenticated") router.push("/login");
  }
});

function open(app) {
  router.push("/apps/" + app.key);
}

async function logout() {
  try {
    await api("/system/auth/logout", { method: "POST" });
  } catch (e) {
    // ignore network errors on logout
  }
  auth.setToken("");
  localStorage.removeItem("moduless_user");
  router.push("/login");
}
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">Moduless</div>
      <nav>
        <router-link class="nav-item" to="/" :class="{ active: route.path === '/' }">概览</router-link>
        <div class="nav-label">扩展模块</div>
        <a
          v-for="app in apps"
          :key="app.key"
          class="nav-item"
          :class="{ active: route.path === '/apps/' + app.key }"
          @click="open(app)"
        >
          <span class="dot" :class="{ on: app.online }"></span>{{ app.display_name }}
        </a>
        <div v-if="apps.length === 0" class="empty">暂无在线扩展</div>
      </nav>
    </aside>

    <div class="body">
      <header class="topbar">
        <div class="crumb">{{ route.path === "/" ? "概览" : route.params.key }}</div>
        <div class="user">
          <span>{{ user ? user.username : "" }}</span>
          <button @click="logout">退出</button>
        </div>
      </header>
      <main class="content">
        <div v-if="error" class="err">{{ error }}</div>
        <router-view />
        <!-- Persistent qiankun mount point; sub-apps render here under /apps/<key>. -->
        <div id="subapp-container"></div>
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  min-height: 100vh;
}
.sidebar {
  width: 220px;
  background: #111827;
  color: #e5e7eb;
  display: flex;
  flex-direction: column;
}
.brand {
  font-size: 20px;
  font-weight: 700;
  padding: 18px 20px;
  border-bottom: 1px solid #1f2937;
}
nav {
  padding: 12px 8px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.nav-label {
  font-size: 11px;
  color: #6b7280;
  padding: 12px 12px 4px;
  text-transform: uppercase;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  border-radius: 6px;
  color: #cbd5e1;
  text-decoration: none;
  cursor: pointer;
  font-size: 14px;
}
.nav-item:hover {
  background: #1f2937;
}
.nav-item.active {
  background: #2563eb;
  color: #fff;
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #6b7280;
}
.dot.on {
  background: #22c55e;
}
.empty {
  color: #6b7280;
  font-size: 13px;
  padding: 8px 12px;
}
.body {
  flex: 1;
  display: flex;
  flex-direction: column;
  background: #f3f4f6;
}
.topbar {
  height: 56px;
  background: #fff;
  border-bottom: 1px solid #e5e7eb;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 20px;
}
.crumb {
  font-weight: 600;
}
.user {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 14px;
  color: #374151;
}
.user button {
  padding: 6px 12px;
  border: 1px solid #d1d5db;
  background: #fff;
  border-radius: 6px;
  cursor: pointer;
}
.content {
  flex: 1;
  padding: 20px;
}
.err {
  color: #dc2626;
  margin-bottom: 12px;
}
</style>
