<script setup>
// Home / overview page shown at "/". The sub-app container lives in the Layout,
// so this view simply welcomes the operator.
const user = JSON.parse(localStorage.getItem("moduless_user") || "null");
</script>

<template>
  <div class="home">
    <h2>欢迎，{{ user ? user.username : "" }}</h2>
    <p>从左侧选择一个扩展模块进入。每个模块是独立部署的微应用，由 Core 通过反向隧道托管。</p>
  </div>
</template>

<style scoped>
.home {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  padding: 24px;
}
.home h2 {
  margin: 0 0 8px;
}
.home p {
  color: #6b7280;
  margin: 0;
}
</style>
