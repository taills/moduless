<script setup>
// Recursive menu tree. Each node is either:
//   - a leaf (entry != ""): clicking routes to /apps<path> and lets qiankun
//     mount the micro-app registered for that path.
//   - a branch (entry == ""): clicking toggles expanded/collapsed in place;
//     the underlying children are still navigable.
//
// Nodes also carry an optional Roles list (set by Core when filtering by
// user role); the host already pre-filters, so this is informational only.
import { ref } from "vue";
import { useRoute } from "vue-router";

const props = defineProps({
  nodes: { type: Array, required: true },
  depth: { type: Number, default: 0 },
});

const route = useRoute();
const open = ref({}); // path → bool

function isActive(path) {
  return route.path === "/apps" + path || route.path.startsWith("/apps" + path + "/");
}

function isLeaf(node) {
  return !!node.entry || (node.children || []).length === 0;
}

function toggle(path) {
  open.value[path] = !open.value[path];
}
</script>

<template>
  <ul :class="['menu-tree', 'depth-' + depth]">
    <li v-for="node in nodes" :key="node.path" class="menu-node">
      <div :class="['row', { active: isActive(node.path), branch: !isLeaf(node) }]">
        <router-link
          v-if="isLeaf(node)"
          :to="'/apps' + node.path"
          class="link"
          :title="node.title"
        >
          <span class="label">{{ node.title }}</span>
        </router-link>
        <button
          v-else
          type="button"
          class="branch-btn"
          @click="toggle(node.path)"
          :title="node.title"
        >
          <span :class="['caret', { open: open[node.path] }]">▸</span>
          <span class="label">{{ node.title }}</span>
        </button>
      </div>
      <MenuTree
        v-if="!isLeaf(node) && open[node.path]"
        :nodes="node.children"
        :depth="depth + 1"
      />
    </li>
  </ul>
</template>

<style scoped>
.menu-tree {
  list-style: none;
  padding: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.menu-tree.depth-0 {
  /* depth-0 sits under the "扩展模块" group label */
}
.menu-tree:not(.depth-0) {
  padding-left: 14px;
  border-left: 1px solid #1f2937;
  margin-left: 12px;
}
.row {
  display: flex;
  align-items: center;
  border-radius: 6px;
  padding: 0 6px;
}
.row:hover {
  background: #1f2937;
}
.row.active {
  background: #2563eb;
}
.row.active .label,
.row.active .caret {
  color: #fff;
}
.link,
.branch-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 8px;
  width: 100%;
  background: transparent;
  border: 0;
  color: #cbd5e1;
  text-decoration: none;
  cursor: pointer;
  font-size: 14px;
  text-align: left;
}
.caret {
  display: inline-block;
  width: 12px;
  color: #6b7280;
  font-size: 11px;
  transition: transform 0.1s linear;
}
.caret.open {
  transform: rotate(90deg);
}
.label {
  flex: 1;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>