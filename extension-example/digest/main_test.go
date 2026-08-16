package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// The whole job is covered here without a Core, a database, or object storage,
// because the three capabilities it uses are behind seams. That is the point of
// the example as much as the archiving is.
//
// Note that the job logs and records a metric on its successful path. Before
// sdk.Log became safe on a nil receiver that alone would have made every test
// below a segmentation fault.

// --- fakes --------------------------------------------------------------

// liveFetcher performs a real HTTP round trip against a test server, so the
// body reading and Content-Type handling are exercised rather than simulated.
// It exists only because sdk.HTTPClient.Get takes a context and http.Client.Get
// does not; everything else lines up.
type liveFetcher struct{ client *http.Client }

func (f liveFetcher) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return f.client.Do(req)
}

type storedFile struct {
	name string
	mime string
	body []byte
}

type memFiles struct{ files []storedFile }

func (m *memFiles) Put(_ context.Context, name, mime string, r io.Reader) (string, int64, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	m.files = append(m.files, storedFile{name: name, mime: mime, body: body})
	return fmt.Sprintf("file-%d", len(m.files)), int64(len(body)), nil
}

// memIndex round-trips through JSON so the documents behave the way Core's
// store does — a struct written and read back is not the same pointer.
type memIndex struct{ docs map[string][]byte }

func newIndex() *memIndex { return &memIndex{docs: map[string][]byte{}} }

func (m *memIndex) Put(_ context.Context, collection, id string, value any) (int64, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	m.docs[collection+"/"+id] = raw
	return 1, nil
}

func (m *memIndex) Get(_ context.Context, collection, id string, dest any) (bool, int64, error) {
	raw, ok := m.docs[collection+"/"+id]
	if !ok {
		return false, 0, nil
	}
	return true, 1, json.Unmarshal(raw, dest)
}

// --- harness ------------------------------------------------------------

type harness struct {
	a     *archiver
	files *memFiles
	index *memIndex
	body  string
	code  int
	hits  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{files: &memFiles{}, index: newIndex(), body: "first", code: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.code)
		_, _ = io.WriteString(w, h.body)
	}))
	t.Cleanup(srv.Close)

	h.a = &archiver{
		source: srv.URL,
		http:   liveFetcher{client: srv.Client()},
		files:  h.files,
		index:  h.index,
	}
	return h
}

func (h *harness) run(t *testing.T, scheduled int64) error {
	t.Helper()
	return h.a.archive(context.Background(), &sdk.Job{Name: "archive", Scheduled: scheduled})
}

// --- tests --------------------------------------------------------------

func TestTheFirstRunArchivesWhatItFetched(t *testing.T) {
	h := newHarness(t)

	if err := h.run(t, 1700000000); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if len(h.files.files) != 1 {
		t.Fatalf("stored %d files, want 1", len(h.files.files))
	}
	got := h.files.files[0]
	if string(got.body) != "first" {
		t.Errorf("stored %q, want what the source served", got.body)
	}
	if got.mime != "application/json" {
		t.Errorf("mime = %q; a snapshot with the wrong type downloads as the wrong thing", got.mime)
	}
	// The filename carries the occurrence the run was for, not the wall clock.
	if !strings.Contains(got.name, "1700000000") {
		t.Errorf("name = %q, want job.Scheduled in it", got.name)
	}

	var rec snapshot
	for key, raw := range h.index.docs {
		if strings.HasSuffix(key, latestID) {
			continue
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("the indexed snapshot is not readable: %v", err)
		}
	}
	if rec.FileID != "file-1" {
		t.Errorf("the snapshot does not point at the file that was stored: %+v", rec)
	}
	if rec.TakenAt != 1700000000 {
		t.Errorf("taken_at = %d, want job.Scheduled", rec.TakenAt)
	}
	if rec.Bytes != int64(len("first")) {
		t.Errorf("bytes = %d, want %d", rec.Bytes, len("first"))
	}
}

// The reason this example has any logic at all. A scheduled job that stores a
// file every run accumulates identical copies forever, and the storage bill is
// the first anyone hears about it.
func TestUnchangedContentIsNotArchivedAgain(t *testing.T) {
	h := newHarness(t)

	if err := h.run(t, 1); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := h.run(t, 2); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if h.hits != 2 {
		t.Errorf("fetched %d times, want 2: skipping the store must not skip the check", h.hits)
	}
	if len(h.files.files) != 1 {
		t.Errorf("stored %d files for identical content, want 1", len(h.files.files))
	}
}

func TestChangedContentIsArchivedAgain(t *testing.T) {
	h := newHarness(t)

	if err := h.run(t, 1); err != nil {
		t.Fatalf("first run: %v", err)
	}
	h.body = "second"
	if err := h.run(t, 2); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if len(h.files.files) != 2 {
		t.Fatalf("stored %d files, want 2", len(h.files.files))
	}
	if string(h.files.files[1].body) != "second" {
		t.Errorf("stored %q, want the changed content", h.files.files[1].body)
	}
}

// A restart must not re-archive. The last digest lives in the store rather than
// in a field precisely so that it survives one.
func TestARestartDoesNotReArchive(t *testing.T) {
	h := newHarness(t)

	if err := h.run(t, 1); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Same store, new archiver: what a restart looks like from here.
	restarted := &archiver{source: h.a.source, http: h.a.http, files: h.files, index: h.index}
	if err := restarted.archive(context.Background(), &sdk.Job{Scheduled: 2}); err != nil {
		t.Fatalf("after restart: %v", err)
	}

	if len(h.files.files) != 1 {
		t.Errorf("stored %d files across a restart, want 1", len(h.files.files))
	}
}

func TestAnUpstreamErrorStoresNothing(t *testing.T) {
	h := newHarness(t)
	h.code = http.StatusBadGateway

	err := h.run(t, 1)
	if err == nil {
		t.Fatal("a 502 from the source was reported as a successful archive")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the upstream status in it", err)
	}
	if len(h.files.files) != 0 {
		t.Errorf("stored %d files from a failed fetch", len(h.files.files))
	}
}

// An unconfigured source is an operator who has not finished setting up, not a
// failure to retry on every tick until someone notices the error log.
func TestAnUnconfiguredSourceIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.a.source = ""

	if err := h.run(t, 1); err != nil {
		t.Errorf("err = %v, want nil for a plugin nobody has configured yet", err)
	}
	if h.hits != 0 {
		t.Errorf("fetched %d times with no source configured", h.hits)
	}
}

func TestABodyOverTheCapFailsRatherThanTruncating(t *testing.T) {
	h := newHarness(t)
	h.body = strings.Repeat("x", maxBody+1)

	err := h.run(t, 1)
	if err == nil {
		t.Fatal("an oversized body was archived; a truncated snapshot is worse than none")
	}
	if len(h.files.files) != 0 {
		t.Errorf("stored %d files from an oversized body", len(h.files.files))
	}
}

// The claim in the package comment, checked by the compiler rather than by
// reading: the SDK clients satisfy these seams with no adapter, which is what
// makes the pattern cheap enough to be worth recommending.
var (
	_ filestore = (*sdk.FilesClient)(nil)
	_ index     = (*sdk.DBClient)(nil)
	_ fetcher   = (*sdk.HTTPClient)(nil)
)
