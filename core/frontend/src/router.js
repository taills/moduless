import { createRouter, createWebHistory } from "vue-router";
import { auth } from "./api";
import Layout from "./components/Layout.vue";
import Login from "./views/Login.vue";
import Home from "./views/Home.vue";
import AppView from "./views/AppView.vue";
import Users from "./views/Users.vue";
import Extensions from "./views/Extensions.vue";
import Plugins from "./views/Plugins.vue";

const router = createRouter({
  history: createWebHistory("/"),
  routes: [
    { path: "/login", name: "login", component: Login },
    {
      path: "/",
      component: Layout,
      children: [
        { path: "", name: "home", component: Home },
        // AppView owns the micro-app lifecycle for whatever menu node this
        // path maps to, mounting and unmounting it by hand so a disabled
        // plugin's UI can be torn down immediately.
        {
          path: "apps/:pathMatch(.*)*",
          name: "app",
          component: AppView,
        },
        { path: "system/plugins", name: "plugins", component: Plugins },
        { path: "system/users", name: "users", component: Users },
        { path: "system/extensions", name: "extensions", component: Extensions },
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
