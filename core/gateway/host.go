package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// hostPlaceholder is shown when the host app has not been built yet.
const hostPlaceholder = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8">` +
	`<title>Moduless</title></head><body style="font-family:system-ui;max-width:640px;margin:64px auto;padding:0 16px">` +
	`<h1>Moduless Core</h1><p>主应用尚未构建。请在 <code>core/frontend</code> 执行 <code>npm install &amp;&amp; npm run build</code>，` +
	`或设置 <code>HOST_FRONTEND_DIR</code> 指向已构建的 dist 目录。</p>` +
	`<p>API 健康检查：<a href="/healthz">/healthz</a></p></body></html>`

// NewHostHandler serves the qiankun host app from dir with SPA fallback: any
// path that is not an existing file falls back to index.html so client-side
// routing works. When the build is missing, a placeholder page is served.
func NewHostHandler(dir string) http.Handler {
	return &hostHandler{dir: dir}
}

type hostHandler struct{ dir string }

func (h *hostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	index := filepath.Join(h.dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(hostPlaceholder))
		return
	}

	// Resolve the request path to a file under dir, guarding against traversal.
	clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	target := filepath.Join(h.dir, clean)
	root := filepath.Clean(h.dir)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		http.ServeFile(w, r, target)
		return
	}
	// SPA fallback: unknown route -> index.html for client-side routing.
	http.ServeFile(w, r, index)
}
