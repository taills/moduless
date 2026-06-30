<script setup>
// Root component: just the router outlet. The Layout component (with sidebar and
// the persistent qiankun container) wraps all authenticated routes; /login
// renders bare.
</script>

<template>
  <router-view />
</template>

<style>
* {
  box-sizing: border-box;
}
body {
  margin: 0;
  font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  color: #1f2329;
}
</style>
