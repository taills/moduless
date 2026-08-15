<script setup>
// The form an operator edits a plugin's settings in.
//
// Rendered from what the plugin declares rather than as a free-text key/value
// editor: the manifest states each setting's label, type and default, so a
// misspelled key is refused by Core instead of sitting in the database looking
// configured while the plugin goes on using its default.
import { onMounted, ref } from "vue";
import { api } from "../api";

const props = defineProps({ pluginKey: { type: String, required: true } });

const declared = ref([]);
const values = ref({});
const error = ref("");
const notice = ref("");
const busy = ref(false);
const loaded = ref(false);

async function load() {
  try {
    const data = await api(`/system/plugins/${props.pluginKey}/config`);
    declared.value = data.declared || [];
    values.value = { ...(data.values || {}) };
    // A setting nobody has configured shows its declared default, so the form
    // says what is actually in effect rather than an empty box.
    for (const d of declared.value) {
      if (values.value[d.key] === undefined) values.value[d.key] = d.default ?? "";
    }
    error.value = "";
  } catch (e) {
    error.value = e.message;
  } finally {
    loaded.value = true;
  }
}

async function save() {
  busy.value = true;
  notice.value = "";
  try {
    const data = await api(`/system/plugins/${props.pluginKey}/config`, {
      method: "POST",
      body: JSON.stringify({ values: values.value }),
    });
    values.value = { ...(data.values || {}) };
    // Saved and pushed are different outcomes, and Core reports them
    // separately: a value that is stored but undelivered needs someone to look
    // at the plugin, not to press save again.
    notice.value = data.warning || "已保存，并推送给运行中的插件";
    error.value = "";
  } catch (e) {
    error.value = e.message;
  } finally {
    busy.value = false;
  }
}

function inputType(decl) {
  if (decl.secret) return "password";
  if (decl.type === "int" || decl.type === "number") return "number";
  return "text";
}

onMounted(load);
</script>

<template>
  <div class="config">
    <p v-if="error" class="err">{{ error }}</p>
    <p v-else-if="loaded && !declared.length" class="muted">这个插件没有声明任何可配置项。</p>

    <div v-for="d in declared" :key="d.key" class="field">
      <label :for="`cfg-${pluginKey}-${d.key}`">
        {{ d.label || d.key }}
        <span v-if="d.required" class="req">必填</span>
        <code>{{ d.key }}</code>
      </label>
      <input
        :id="`cfg-${pluginKey}-${d.key}`"
        v-model="values[d.key]"
        :type="inputType(d)"
        :placeholder="d.default"
      />
      <p v-if="d.description" class="desc">{{ d.description }}</p>
    </div>

    <div v-if="declared.length" class="row">
      <button :disabled="busy" @click="save">保存</button>
      <span v-if="notice" class="notice">{{ notice }}</span>
    </div>
  </div>
</template>

<style scoped>
.config {
  padding: 8px 0 4px;
  max-width: 520px;
}
.field {
  margin-bottom: 12px;
}
label {
  display: block;
  font-size: 13px;
  color: #374151;
  margin-bottom: 4px;
}
label code {
  background: #f3f4f6;
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 12px;
  color: #6b7280;
  margin-left: 6px;
}
.req {
  color: #b45309;
  font-size: 12px;
  margin-left: 4px;
}
input {
  width: 100%;
  padding: 6px 8px;
  border: 1px solid #d1d5db;
  border-radius: 4px;
}
.desc {
  margin: 4px 0 0;
  font-size: 12px;
  color: #6b7280;
}
.row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.notice {
  font-size: 12px;
  color: #4b5563;
}
.muted {
  color: #6b7280;
}
.err {
  color: #b91c1c;
}
</style>
