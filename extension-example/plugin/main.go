// Command notes is a complete example plugin.
//
// It shows the four things a plugin can do: serve its own HTTP API, store data
// through Core, intercept requests with a filter, and run scheduled work.
//
// Build it and drop it in Core's plugin directory:
//
//	CGO_ENABLED=0 go build -o notes/bin/plugin ./extension-example/plugin
//	cp extension-example/plugin/manifest.yaml notes/
//	PLUGIN_DIR=$(pwd) go run ./core
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// Note is one stored document.
type Note struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Author  string `json:"author"`
	Created string `json:"created"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /notes", listNotes)
	mux.HandleFunc("POST /notes", createNote)
	mux.HandleFunc("GET /notes/{id}", getNote)
	mux.HandleFunc("DELETE /notes/{id}", deleteNote)
	mux.HandleFunc("GET /stats", stats)

	// Nothing here writes to stdout: Core reads the startup handshake from the
	// plugin's first stdout line, so a stray fmt.Println would stop it booting.
	log.SetPrefix("[notes] ")

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePreRoute: rateLimit,
			sdk.PhaseLog:      accessLog,
		},
		Jobs: map[string]sdk.JobFunc{
			"nightly-summary": nightlySummary,
		},
		OnShutdown: func(context.Context) error {
			log.Print("draining")
			return nil
		},
	})
}

// --- HTTP API ---------------------------------------------------------------

func listNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	q := sdk.DB.Where("notes").SortDesc("created").Limit(50)
	if author := r.URL.Query().Get("author"); author != "" {
		q = q.Eq("author", author)
	}
	if after := r.URL.Query().Get("after"); after != "" {
		q = q.After(after)
	}

	var notes []Note
	next, err := q.All(ctx, &notes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes, "next": next})
}

func createNote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var note Note
	if err := json.NewDecoder(r.Body).Decode(&note); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if note.Title == "" {
		httpError(w, http.StatusBadRequest, fmt.Errorf("title is required"))
		return
	}

	// The caller Core authenticated. A plugin never has to parse a token.
	if u := sdk.User(ctx); u != nil {
		note.Author = u.Username
	}
	note.ID = fmt.Sprintf("note-%d", time.Now().UnixNano())
	note.Created = time.Now().UTC().Format(time.RFC3339)

	if _, err := sdk.DB.Put(ctx, "notes", note.ID, note); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	// Best-effort broadcast. Anything that must not be lost would go on the
	// durable queue instead.
	_ = sdk.Events.Publish(ctx, "note.created", note)

	sdk.Log.Info(ctx, "note created", "id", note.ID, "author", note.Author)
	writeJSON(w, http.StatusCreated, note)
}

func getNote(w http.ResponseWriter, r *http.Request) {
	var note Note
	found, _, err := sdk.DB.Get(r.Context(), "notes", r.PathValue("id"), &note)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		httpError(w, http.StatusNotFound, fmt.Errorf("note not found"))
		return
	}
	writeJSON(w, http.StatusOK, note)
}

func deleteNote(w http.ResponseWriter, r *http.Request) {
	if _, err := sdk.DB.Delete(r.Context(), "notes", r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stats aggregates in the database rather than pulling every row back, which
// is the point of having aggregation in the store at all.
func stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	total, err := sdk.DB.Where("notes").Count(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total})
}

// --- Filters ----------------------------------------------------------------

var requestCount atomic.Int64

// rateLimit runs on every request the manifest points at it, including ones
// served by Core or by other plugins. That reach is what makes filters useful
// for cross-cutting concerns, and why the manifest scopes them by path.
func rateLimit(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	if requestCount.Add(1)%1000 == 0 {
		sdk.Log.Warn(ctx, "request volume checkpoint", "count", fmt.Sprint(requestCount.Load()))
	}

	// A crude example: refuse a request that asks to be refused.
	if req.Header.Get("X-Simulate-Overload") == "1" {
		return sdk.Stop(http.StatusTooManyRequests, []byte("slow down")).
			WithHeader("Retry-After", "5"), nil
	}

	// Pass something to later filters and to the backend.
	return sdk.Mutate().SetValue("seen-by", "notes-plugin"), nil
}

// accessLog runs after the response has been sent, so nothing it does can slow
// the request down.
func accessLog(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	sdk.Log.Info(ctx, "request",
		"method", req.Method,
		"path", req.Path,
		"status", fmt.Sprint(req.ResponseStatus),
	)
	return sdk.Continue(), nil
}

// --- Scheduled work ---------------------------------------------------------

// nightlySummary is invoked by Core on the schedule in manifest.yaml. Core owns
// the schedule, so the job stops when the plugin is disabled and only one
// replica runs each occurrence.
func nightlySummary(ctx context.Context, job *sdk.Job) error {
	byAuthor, err := sdk.DB.Where("notes").Count(ctx)
	if err != nil {
		return fmt.Errorf("count notes: %w", err)
	}
	sdk.Log.Info(ctx, "nightly summary", "job", job.Name, "notes", fmt.Sprint(byAuthor))

	// Hand the heavy part to the queue so a slow summary does not hold the job
	// slot open.
	_, _, err = sdk.Queue.Publish(ctx, "summaries", map[string]any{
		"generated_at": time.Now().UTC(),
		"note_count":   byAuthor,
	}, sdk.WithDedupKey(fmt.Sprintf("summary-%d", job.Scheduled)))
	return err
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(err.Error())})
}
