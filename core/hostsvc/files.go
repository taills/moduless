package hostsvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/storage"
)

// ObjectStore is the slice of the object-storage client this needs.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, mime string) error
}

// Files gives plugins the ability to produce and remove files, on top of the
// download-token flow that already existed.
//
// Binary content still never travels through the plugin transport for reads:
// a plugin asks for a short-lived download URL and the browser fetches from
// Core directly. Writes do pass through, because a plugin generating a report
// has nowhere else to put the bytes, so they are bounded by MaxUploadBytes.
type Files struct {
	conn  *sql.DB
	q     *sqlc.Queries
	store ObjectStore

	// MaxUploadBytes caps a single plugin-written file. Without a ceiling a
	// plugin could stream indefinitely into object storage on Core's
	// credentials.
	MaxUploadBytes int64

	// PublicBaseURL prefixes generated download URLs when Core sits behind a
	// known origin. Empty yields a path-only URL, which is what the console
	// wants for same-origin downloads.
	PublicBaseURL string
}

// DefaultMaxUploadBytes bounds a plugin-written file.
const DefaultMaxUploadBytes = 64 << 20

func NewFiles(conn *sql.DB, q *sqlc.Queries, store *storage.RustFSClient) *Files {
	f := &Files{conn: conn, q: q, MaxUploadBytes: DefaultMaxUploadBytes}
	// A nil *RustFSClient in an interface is not a nil interface, so the
	// conversion is guarded rather than assigned blindly.
	if store != nil {
		f.store = store
	}
	return f
}

func (f *Files) maxUpload() int64 {
	if f.MaxUploadBytes > 0 {
		return f.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

// Put streams a file into object storage and records it.
//
// The storage key is namespaced by plugin so one plugin's objects are
// distinguishable in the bucket, and the file id is a UUID rather than
// anything caller-supplied: a plugin-chosen id would let one plugin overwrite
// another's record.
func (f *Files) Put(ctx context.Context, pluginKey, filename, mimeType string, r io.Reader) (string, int64, error) {
	if f.store == nil || f.q == nil {
		return "", 0, errors.New("file storage is not configured")
	}
	if filename == "" {
		filename = "untitled"
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Read through a limit that is one byte over the ceiling, so exceeding it
	// is detectable rather than silently truncating the file.
	limited := io.LimitReader(r, f.maxUpload()+1)
	var buf bytes.Buffer
	size, err := io.Copy(&buf, limited)
	if err != nil {
		return "", 0, fmt.Errorf("read upload: %w", err)
	}
	if size > f.maxUpload() {
		return "", 0, fmt.Errorf("file exceeds the %d byte limit", f.maxUpload())
	}

	fileID := uuid.NewString()
	storageKey := "plugins/" + pluginKey + "/" + fileID

	if err := f.store.PutObject(ctx, storageKey, bytes.NewReader(buf.Bytes()), size, mimeType); err != nil {
		return "", 0, fmt.Errorf("store object: %w", err)
	}

	if err := f.q.InsertFile(ctx, sqlc.InsertFileParams{
		ID:         fileID,
		Filename:   filename,
		Size:       size,
		MimeType:   mimeType,
		StorageKey: storageKey,
		// Written by a plugin rather than a user, so there is no uploader.
		UploaderID: "",
	}); err != nil {
		// The object is already written. Leaving it orphaned is preferable to
		// reporting success for a file no record points at; a storage sweep
		// can reconcile, a phantom file id cannot.
		return "", 0, fmt.Errorf("record file: %w", err)
	}
	return fileID, size, nil
}

// Delete removes a plugin's file record.
//
// The object itself is left for a storage lifecycle rule rather than deleted
// inline: a failed object delete after a successful record delete would leave
// the system unable to find or clean the object at all.
func (f *Files) Delete(ctx context.Context, pluginKey, fileID string) error {
	if f.q == nil {
		return errors.New("file storage is not configured")
	}
	row, err := f.q.GetFile(ctx, fileID)
	if err != nil {
		return fmt.Errorf("file not found")
	}
	if !ownsFile(pluginKey, row.StorageKey) {
		// Reported as not-found rather than forbidden: telling a plugin that a
		// file exists but belongs to someone else is itself information.
		return fmt.Errorf("file not found")
	}
	if f.conn == nil {
		return errors.New("file storage is not configured")
	}
	if _, err := f.conn.ExecContext(ctx, "DELETE FROM system_files WHERE id = $1", fileID); err != nil {
		return fmt.Errorf("delete file record: %w", err)
	}
	return nil
}

// GenerateDownloadToken mints a short-lived, single-use download URL.
func (f *Files) GenerateDownloadToken(ctx context.Context, pluginKey, fileID, userID string, expiry time.Duration) (string, time.Time, error) {
	if f.q == nil {
		return "", time.Time{}, errors.New("file storage is not configured")
	}
	if expiry <= 0 {
		expiry = 5 * time.Minute
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(b)
	expiresAt := time.Now().Add(expiry)

	if err := f.q.InsertDownloadToken(ctx, sqlc.InsertDownloadTokenParams{
		Token:     token,
		FileID:    fileID,
		UserID:    userID,
		ExpiresAt: expiresAt,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("issue download token: %w", err)
	}

	// Clean path parameters, never a query string: the download route is
	// specified to reject anything after a "?".
	url := f.PublicBaseURL + "/api/system/files/download/" + fileID + "/" + token
	return url, expiresAt, nil
}

// Metadata reports what Core knows about a file.
func (f *Files) Metadata(ctx context.Context, pluginKey, fileID string) (FileMeta, error) {
	if f.q == nil {
		return FileMeta{}, errors.New("file storage is not configured")
	}
	row, err := f.q.GetFile(ctx, fileID)
	if err != nil {
		return FileMeta{Found: false}, nil
	}
	return FileMeta{
		Found:    true,
		FileID:   row.ID,
		Filename: row.Filename,
		Size:     row.Size,
		MimeType: row.MimeType,
	}, nil
}

// ownsFile reports whether a storage key belongs to a plugin. Files uploaded
// by users through the browser are not owned by any plugin.
func ownsFile(pluginKey, storageKey string) bool {
	return len(storageKey) > len("plugins/"+pluginKey+"/") &&
		storageKey[:len("plugins/"+pluginKey+"/")] == "plugins/"+pluginKey+"/"
}
