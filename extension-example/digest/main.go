// Command digest archives an external resource whenever it changes.
//
// It is the eighth example and it exists for the three capabilities the other
// seven never touch: outbound HTTP, writing files, and handing out download
// links. On a schedule it fetches a URL through Core's egress proxy, hashes
// what came back, and — only if the content differs from last time — stores it
// as a file and indexes it.
//
// # The thing worth copying from here
//
// Core refuses plugin egress to loopback and private addresses. That is correct
// (it is what stops an allow-listed hostname resolving to 169.254.169.254) but
// it means an author cannot point sdk.HTTP at a test server: there is no
// address a test can listen on that Core will dial. The same is true of every
// other capability for a different reason — sdk.Files talks to Core, and under
// `go test` there is no Core.
//
// So the capabilities are behind seams. Each is one or two methods, and each is
// satisfied by the SDK client directly, with no adapter, because the SDK deals
// in *http.Response, io.Reader and plain strings rather than types of its own:
//
//	var _ fetcher = sdk.HTTP     // compiles
//	var _ filestore = sdk.Files  // compiles
//	var _ index = sdk.DB         // compiles
//
// main() wires the real ones; main_test.go wires an httptest.Server and two
// recording fakes, and the whole job — including "unchanged, so archive
// nothing", which is the only interesting logic here — is covered without Core.
//
// Build:
//
//	CGO_ENABLED=0 go build -o bin/digest .
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

const (
	collection = "snapshots"
	// latestID names the one document holding the hash most recently archived.
	// Keeping it in the store rather than in a field means a restart does not
	// re-archive content that has not changed.
	latestID = "_latest"
	// A cap, because the body is read into memory to be hashed. Beyond this the
	// job fails loudly rather than the process dying somewhere less obvious.
	maxBody       = 8 << 20
	downloadValid = 5 * time.Minute
)

// --- the seams ----------------------------------------------------------

type fetcher interface {
	Get(ctx context.Context, url string) (*http.Response, error)
}

type filestore interface {
	Put(ctx context.Context, filename, mimeType string, r io.Reader) (string, int64, error)
}

type index interface {
	Put(ctx context.Context, collection, id string, value any) (int64, error)
	Get(ctx context.Context, collection, id string, dest any) (bool, int64, error)
}

type archiver struct {
	source string
	http   fetcher
	files  filestore
	index  index
}

type snapshot struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	FileID  string `json:"file_id"`
	TakenAt int64  `json:"taken_at"`
}

type marker struct {
	SHA256 string `json:"sha256"`
}

func main() {
	a := &archiver{http: sdk.HTTP, files: sdk.Files, index: sdk.DB}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /snapshots", a.list)
	mux.HandleFunc("GET /snapshots/{id}/download", a.download)

	sdk.Serve(sdk.Config{
		Handler: mux,
		Jobs: map[string]sdk.JobFunc{
			"archive": a.archive,
		},
		OnConfigChanged: a.configure,
	})
}

func (a *archiver) configure(cfg map[string]string) {
	a.source = cfg["source_url"]
}

// --- the job ------------------------------------------------------------

// archive fetches the source and stores it if it has changed.
//
// The comparison is the point: a scheduled job that writes a file every run
// accumulates identical copies forever, and the storage bill is the first
// anyone hears about it.
func (a *archiver) archive(ctx context.Context, job *sdk.Job) error {
	if a.source == "" {
		// Not an error worth retrying: an operator has not configured this yet.
		sdk.Log.Warn(ctx, "no source_url configured, nothing to archive")
		return nil
	}

	resp, err := a.http.Get(ctx, a.source)
	if err != nil {
		// Core distinguishes these on the wire — PermissionDenied for a host
		// outside egress_allow, Unavailable for a host that would not answer —
		// so a caller that cares can branch. Here the job simply fails and Core
		// runs it again on the next tick.
		return fmt.Errorf("fetching %s: %w", a.source, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", a.source, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", a.source, err)
	}
	if len(body) > maxBody {
		return fmt.Errorf("%s returned more than %d bytes", a.source, maxBody)
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	var prev marker
	found, _, err := a.index.Get(ctx, collection, latestID, &prev)
	if err != nil {
		return fmt.Errorf("reading the last digest: %w", err)
	}
	if found && prev.SHA256 == digest {
		sdk.Log.Info(ctx, "source unchanged, nothing archived",
			"sha256", digest, "bytes", len(body))
		return nil
	}

	// job.Scheduled, not time.Now(): it is the occurrence this run is for, and
	// it stays correct when Core was busy and the job ran late.
	taken := job.Scheduled

	fileID, size, err := a.files.Put(ctx,
		fmt.Sprintf("digest-%d.bin", taken), contentType(resp), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("storing the snapshot: %w", err)
	}

	rec := snapshot{
		ID: sdk.NewID(), Source: a.source, SHA256: digest,
		Bytes: size, FileID: fileID, TakenAt: taken,
	}
	if _, err := a.index.Put(ctx, collection, rec.ID, rec); err != nil {
		return fmt.Errorf("indexing the snapshot: %w", err)
	}
	// Last, so a crash between the two leaves a snapshot that will simply be
	// taken again rather than a marker pointing at content nobody stored.
	if _, err := a.index.Put(ctx, collection, latestID, marker{SHA256: digest}); err != nil {
		return fmt.Errorf("recording the digest: %w", err)
	}

	sdk.Log.Info(ctx, "archived a changed source",
		"sha256", digest, "bytes", size, "file", fileID)
	sdk.Log.Metric(ctx, "digest_snapshots_stored", 1, nil)
	return nil
}

// contentType keeps whatever the source said, falling back to something
// unambiguous. A snapshot with the wrong type downloads as the wrong thing.
func contentType(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// --- the API ------------------------------------------------------------

// list returns recent snapshots, newest first.
//
// This one is not behind a seam: sdk.DB.Where returns a concrete fluent builder
// rather than an interface, so faking it would mean reimplementing the builder.
// That is the one part of the host surface without a natural seam, and it is
// why this handler has no unit test while the job has several.
//
// IsNotNull("file_id") rather than Ne("id", latestID), which would also have
// worked — but for a reason worth not relying on. A predicate compiles to
// `<jsonb path> != $1`, and the path yields SQL NULL for a document without the
// field, so `NULL != '_latest'` is NULL and the marker drops out. Correct, and
// the opposite of what most readers expect a Ne to mean. A snapshot is a
// document that points at a stored file; saying that is clearer than relying on
// three-valued logic to exclude one that does not.
func (a *archiver) list(w http.ResponseWriter, r *http.Request) {
	var out []snapshot
	if _, err := sdk.DB.Where(collection).
		IsNotNull("file_id").
		SortDesc("taken_at").
		Limit(50).
		All(r.Context(), &out); err != nil {
		http.Error(w, "listing snapshots", http.StatusInternalServerError)
		sdk.Log.Error(r.Context(), "listing snapshots", "err", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": out})
}

// download hands back a short-lived URL rather than the bytes.
//
// Binary content does not travel back through the plugin transport: the browser
// fetches it from Core directly. That is why files:read exists as a separate
// permission from files:write — this plugin writes snapshots and mints links,
// and does not read a single byte back.
func (a *archiver) download(w http.ResponseWriter, r *http.Request) {
	var rec snapshot
	found, _, err := a.index.Get(r.Context(), collection, r.PathValue("id"), &rec)
	if err != nil {
		http.Error(w, "reading the snapshot", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such snapshot", http.StatusNotFound)
		return
	}

	user := sdk.User(r.Context())
	if user == nil {
		// A download token is minted for someone. Anonymous callers get no link
		// rather than a link belonging to nobody.
		http.Error(w, "sign in to download", http.StatusUnauthorized)
		return
	}

	url, expires, err := sdk.Files.DownloadURL(r.Context(), rec.FileID, user.UserID, downloadValid)
	if err != nil {
		http.Error(w, "minting a download link", http.StatusInternalServerError)
		sdk.Log.Error(r.Context(), "minting a download link", "err", err, "file", rec.FileID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url":        url,
		"expires_at": expires.Unix(),
		"expires_in": strconv.Itoa(int(time.Until(expires).Seconds())) + "s",
	})
}
