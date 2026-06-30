import { createRouter, createWebHistory } from "vue-router";
import { auth } from "./api";
import Layout from "./components/Layout.vue";
import Login from "./views/Login.vue";
import Home from "./views/Home.vue";
import AppView from "./views/AppView.vue";

const router = createRouter({
  history: createWebHistory("/"),
  routes: [
    { path: "/login", name: "login", component: Login },
    {
      path: "/",
      component: Layout,
      children: [
        { path: "", name: "home", component: Home },
        // qiankun activeRule "/apps/<key>" mounts the sub-app into the layout's
        // persistent container; this routed view is just a placeholder.
        { path: "apps/:key", name: "app", component: AppView },
      ],
    },
  ],
});

// Auth guard: everything except /login requires a session token.
router.beforeEach((to) => {
  if (to.path !== "/login" && !auth.token) {
    return { path: "/login" };
  }
  if (to.path === "/login" && auth.token) {
    return { path: "/" };
  }
  return true;
});

export default router;
