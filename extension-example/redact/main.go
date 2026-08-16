// Command redact is an example plugin that rewrites other plugins' responses.
//
// It is the one shape the other five do not cover: a filter in the
// post_handler phase, which is where a plugin sees what the system is about to
// say and gets to change it. Everything else either decides whether a request
// proceeds or watches it go by.
//
//	CGO_ENABLED=0 go build -o redact/bin/plugin ./extension-example/redact
//	cp extension-example/redact/manifest.yaml redact/
//	PLUGIN_DIR=$(pwd) go run ./core
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// settings is what an operator configures. Guarded because Core pushes changes
// from a background goroutine while requests are being served.
var settings struct {
	sync.RWMutex
	fields map[string]bool
	mask   string
}

func fieldsToMask() (map[string]bool, string) {
	settings.RLock()
	defer settings.RUnlock()
	return settings.fields, settings.mask
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /settings", showSettings)

	log.SetPrefix("[redact] ")

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePostHandler: redact,
		},
		OnConfigChanged: configure,
	})
}

// configure applies admin settings.
//
// A named function rather than a closure written inline above, because that is
// the difference between this logic being testable and not: a test can call
// this, and it cannot call an anonymous function passed to sdk.Serve. The
// normalisations here — lowercasing field names, defaulting the mask — are
// exactly the kind of thing that is worth a test and easy to get wrong.
func configure(cfg map[string]string) {
	set := map[string]bool{}
	for _, f := range strings.Split(cfg["fields"], ",") {
		if f = strings.TrimSpace(f); f != "" {
			set[strings.ToLower(f)] = true
		}
	}
	mask := cfg["mask"]
	if mask == "" {
		mask = "[redacted]"
	}
	settings.Lock()
	settings.fields, settings.mask = set, mask
	settings.Unlock()
}

// redact removes configured fields from a JSON response.
//
// Three things here are the point of the example, and two of them are traps.
//
// The mechanism: there is no mutation for the response body. `Mutate()` can
// change headers, the path, the identity and context values, and nothing else.
// Rewriting a body is done by short-circuiting from post_handler, where the
// backend has already answered — so a short circuit does not prevent a
// response, it replaces one.
//
// Trap one: a replacement carries only its own headers. Core writes the short
// circuit's headers and drops everything the backend set, so a filter that
// returns Stop without copying them strips Content-Type from every response it
// touches. The symptom is a browser showing JSON as plain text, which points
// nowhere near this plugin.
//
// Trap two: returning Stop when nothing changed is not free and not harmless.
// It costs the header copy above and makes every response in the system pass
// through this plugin's idea of what a response looks like. Continue when
// there is nothing to do.
func redact(_ context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	if !isJSON(req.ResponseHeader) || len(req.ResponseBody) == 0 {
		return sdk.Continue(), nil
	}

	fields, mask := fieldsToMask()
	if len(fields) == 0 {
		return sdk.Continue(), nil
	}

	var doc any
	if err := json.Unmarshal(req.ResponseBody, &doc); err != nil {
		// Not the shape we thought. Passing it through unchanged is the only
		// safe answer: a redactor that fails closed here would take down every
		// endpoint that ever returns something it cannot parse.
		return sdk.Continue(), nil
	}

	cleaned, changed := walk(doc, fields, mask)
	if !changed {
		return sdk.Continue(), nil
	}

	out, err := json.Marshal(cleaned)
	if err != nil {
		// Re-encoding failed after decoding succeeded, which should not
		// happen. Passing the original through would leak exactly what this
		// plugin exists to remove, so this is the one case worth failing on.
		return nil, err
	}

	// Stop replaces the response. Carry the backend's headers across, or they
	// are lost — see trap one.
	res := sdk.Stop(req.ResponseStatus, out)
	for key, values := range req.ResponseHeader {
		for _, v := range values {
			res = res.WithHeader(key, v)
		}
	}
	return res, nil
}

// walk replaces matching fields anywhere in the document, at any depth.
func walk(node any, fields map[string]bool, mask string) (any, bool) {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		out := make(map[string]any, len(v))
		for key, val := range v {
			if fields[strings.ToLower(key)] {
				out[key] = mask
				changed = true
				continue
			}
			sub, subChanged := walk(val, fields, mask)
			out[key] = sub
			changed = changed || subChanged
		}
		return out, changed

	case []any:
		changed := false
		out := make([]any, len(v))
		for i, item := range v {
			sub, subChanged := walk(item, fields, mask)
			out[i] = sub
			changed = changed || subChanged
		}
		return out, changed

	default:
		return node, false
	}
}

func isJSON(h http.Header) bool {
	return strings.Contains(h.Get("Content-Type"), "json")
}

// showSettings lets an operator confirm what is being removed, without having
// to read the console's config form.
func showSettings(w http.ResponseWriter, r *http.Request) {
	if !sdk.User(r.Context()).HasRole("admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fields, mask := fieldsToMask()
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"fields": names, "mask": mask})
}
