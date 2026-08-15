package gateway

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/taills/moduless/core/pluginhost"
)

// PluginAssetPrefix is where a plugin's micro-frontend is served from.
const PluginAssetPrefix = "/plugins/"

// PackageSource resolves a plugin key to its installed package.
// *pluginhost.Manager satisfies it.
type PackageSource interface {
	Package(key string) (*pluginhost.Package, bool)
}

// PluginAssetHandler serves a plugin's built micro-frontend straight from its
// package directory.
//
// The reverse-tunnel model kept these files in memory, streamed up over the
// connection at registration time, which meant a Core restart lost every
// plugin's UI until each extension reconnected and re-uploaded it. Reading
// from the package directory removes that whole failure mode: the assets are
// simply there.
func PluginAssetHandler(src PackageSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, PluginAssetPrefix)
		key, sub, _ := strings.Cut(rest, "/")
		if key == "" {
			http.NotFound(w, r)
			return
		}

		pkg, ok := src.Package(key)
		if !ok || pkg.FrontendDir == "" {
			http.NotFound(w, r)
			return
		}

		// The micro-frontend entry (directory root) maps to index.html so the
		// qiankun host can load /plugins/<key>/ as the sub-app entry.
		if sub == "" {
			sub = "index.html"
		}

		// filepath.Clean on a rooted path collapses any "..", so a request for
		// /plugins/x/../../etc/passwd cannot escape the package directory.
		clean := filepath.Clean("/" + sub)
		full := filepath.Join(pkg.FrontendDir, clean)

		data, err := os.ReadFile(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if ct := mime.TypeByExtension(filepath.Ext(clean)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write(data)
	}
}
