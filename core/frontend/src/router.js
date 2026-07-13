import { createRouter, createWebHistory } from "vue-router";
import { auth } from "./api";
import Layout from "./components/Layout.vue";
import Login from "./views/Login.vue";
import Home from "./views/Home.vue";
import AppView from "./views/AppView.vue";
import Users from "./views/Users.vue";
import Extensions from "./views/Extensions.vue";

const router = createRouter({
  history: createWebHistory("/"),
  routes: [
    { path: "/login", name: "login", component: Login },
    {
      path: "/",
      component: Layout,
      children: [
        { path: "", name: "home", component: Home },
        // qiankun activeRule "/apps<menu.path>" mounts the sub-app into the
        // layout's persistent container. AppView reads route.params.pathMatch
        // to know which menu node is active (used for crumbs / breadcrumbs).
        {
          path: "apps/:pathMatch(.*)*",
          name: "app",
          component: AppView,
        },
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
