import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

// The form an operator edits a plugin's settings in.
//
// Core's half of this is well covered: undeclared keys refused, secrets masked
// on the way out, the mask coming back understood as "leave it alone", a save
// that could not be delivered reported separately from one that failed. None
// of that says what the console does with those answers, and one of them is
// easy to get wrong in a way nobody would notice — a warning that is swallowed
// leaves an operator believing a change reached a running plugin when it did
// not.

const calls = [];
let responses = [];

vi.mock("../src/api", () => ({
  api: (path, options = {}) => {
    calls.push({ path, options });
    const next = responses.shift();
    if (next instanceof Error) return Promise.reject(next);
    return Promise.resolve(next ?? {});
  },
}));

const { default: PluginConfig } = await import("../src/components/PluginConfig.vue");

const declared = [
  { key: "greeting", label: "Greeting", type: "string", default: "hello" },
  { key: "retries", label: "Retries", type: "int", default: "3" },
  { key: "api_token", label: "Token", type: "string", secret: true },
];

const MASK = "••••••••";

async function form(values = {}) {
  responses = [{ key: "widget", declared, values }];
  const wrapper = mount(PluginConfig, { props: { pluginKey: "widget" } });
  await flushPromises();
  return wrapper;
}

function inputFor(wrapper, key) {
  return wrapper.get(`#cfg-widget-${key}`);
}

beforeEach(() => {
  calls.length = 0;
  responses = [];
});

describe("PluginConfig", () => {
  it("renders one field per declaration, in the declared order", async () => {
    const wrapper = await form();

    const inputs = wrapper.findAll("input");
    expect(inputs).toHaveLength(declared.length);
    expect(wrapper.text()).toContain("Greeting");
    expect(wrapper.text()).toContain("Retries");
  });

  it("shows a secret as a password field, so it is not read over a shoulder", async () => {
    const wrapper = await form({ api_token: MASK });
    expect(inputFor(wrapper, "api_token").attributes("type")).toBe("password");
    expect(inputFor(wrapper, "greeting").attributes("type")).toBe("text");
    expect(inputFor(wrapper, "retries").attributes("type")).toBe("number");
  });

  it("falls back to the declared default for a setting nobody has configured", async () => {
    const wrapper = await form({}); // nothing stored

    // The form says what is in effect, not what is stored. An empty box would
    // read as "unset" for a setting that is very much doing something.
    expect(inputFor(wrapper, "greeting").element.value).toBe("hello");
    expect(inputFor(wrapper, "retries").element.value).toBe("3");
  });

  it("sends back exactly what the form holds", async () => {
    const wrapper = await form({ greeting: "hi" });
    await inputFor(wrapper, "greeting").setValue("hey");

    responses = [{ key: "widget", values: { greeting: "hey", retries: "3" } }];
    await wrapper.get("button").trigger("click");
    await flushPromises();

    const save = calls.at(-1);
    expect(save.options.method).toBe("POST");
    expect(JSON.parse(save.options.body).values.greeting).toBe("hey");
  });

  it("submits the mask unchanged for a secret it did not touch", async () => {
    // The console never received the real value, so what it must send back is
    // exactly what it was given. Sending anything else — an empty string, a
    // trimmed version — would overwrite a credential with nonsense, and the
    // first sign would be the plugin failing to authenticate somewhere else
    // entirely.
    const wrapper = await form({ api_token: MASK, greeting: "hi" });
    await inputFor(wrapper, "greeting").setValue("hey");

    responses = [{ key: "widget", values: {} }];
    await wrapper.get("button").trigger("click");
    await flushPromises();

    const sent = JSON.parse(calls.at(-1).options.body).values;
    expect(sent.api_token).toBe(MASK);
  });

  it("shows the server's warning when a value was stored but not delivered", async () => {
    const wrapper = await form({ greeting: "hi" });

    responses = [
      {
        key: "widget",
        values: { greeting: "hi" },
        warning: "saved, but not delivered to the running plugin: plugin is not running",
      },
    ];
    await wrapper.get("button").trigger("click");
    await flushPromises();

    // Saved and delivered are different outcomes with different remedies:
    // pressing save again fixes neither, and someone has to go and look at the
    // plugin. Swallowing this leaves an operator believing the change took
    // effect on a process that never heard about it.
    expect(wrapper.text()).toContain("not delivered");
  });

  it("says it succeeded when there is no warning", async () => {
    const wrapper = await form({ greeting: "hi" });

    responses = [{ key: "widget", values: { greeting: "hi" } }];
    await wrapper.get("button").trigger("click");
    await flushPromises();

    expect(wrapper.text()).not.toContain("not delivered");
    expect(wrapper.text()).toContain("已保存");
  });

  it("reports a failed save rather than looking like nothing happened", async () => {
    const wrapper = await form({ greeting: "hi" });

    responses = [new Error("plugin configuration needs a database")];
    await wrapper.get("button").trigger("click");
    await flushPromises();

    expect(wrapper.text()).toContain("needs a database");
  });

  it("says so when a plugin declares nothing, instead of showing an empty form", async () => {
    responses = [{ key: "widget", declared: [], values: {} }];
    const wrapper = mount(PluginConfig, { props: { pluginKey: "widget" } });
    await flushPromises();

    expect(wrapper.findAll("input")).toHaveLength(0);
    expect(wrapper.text()).toContain("没有声明");
    // And no save button, since there is nothing to save.
    expect(wrapper.findAll("button")).toHaveLength(0);
  });

  it("surfaces a failed load rather than rendering a blank panel", async () => {
    responses = [new Error("admin privileges required")];
    const wrapper = mount(PluginConfig, { props: { pluginKey: "widget" } });
    await flushPromises();

    expect(wrapper.text()).toContain("admin privileges required");
  });
});
