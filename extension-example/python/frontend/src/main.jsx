import React from "react";
import { createRoot } from "react-dom/client";

function App() {
  const [info, setInfo] = React.useState(null);

  React.useEffect(() => {
    fetch("/api/extensions/python_example/info")
      .then((r) => r.json())
      .then(setInfo)
      .catch(() => setInfo({ error: "offline" }));
  }, []);

  return (
    <div style={{ fontFamily: "sans-serif", padding: 16 }}>
      <h2>Python 扩展示例</h2>
      <pre>{JSON.stringify(info, null, 2)}</pre>
    </div>
  );
}

let root;

/** Standalone dev run. */
function render(container) {
  const el = container
    ? container.querySelector("#python-example-root")
    : document.getElementById("python-example-root");
  root = createRoot(el);
  root.render(<App />);
}

// Qiankun micro-frontend lifecycle hooks.
export async function bootstrap() {}
export async function mount(props) {
  render(props && props.container);
}
export async function unmount() {
  if (root) root.unmount();
}

// Render immediately when not running inside the Qiankun host.
if (!window.__POWERED_BY_QIANKUN__) {
  render();
}
