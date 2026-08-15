package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/storage"
)

// maxUploadBytes caps a single multipart upload to 100 MiB.
const maxUploadBytes = 100 << 20

// FileHandler serves the centralized upload + path-param download endpoints.
type FileHandler struct {
	Storage *storage.RustFSClient
	Queries *sqlc.Queries

	// Auth resolves who is uploading. Nil records every upload as anonymous,
	// which is what a Core without a session store can honestly say.
	Auth UserResolver
}

func NewFileHandler(s *storage.RustFSClient, q *sqlc.Queries) *FileHandler {
	return &FileHandler{Storage: s, Queries: q}
}

// uploader reports who is making this request.
//
// Resolved from the session, not from a header. This used to read X-User-Id,
// which nothing in Core ever set and any client could send — so the recorded
// uploader of every file was whatever the uploader chose to call themselves.
func (h *FileHandler) uploader(r *http.Request) string {
	if h.Auth == nil {
		return "anonymous"
	}
	user, ok := h.Auth.Resolve(SessionToken(r))
	if !ok {
		return "anonymous"
	}
	return strconv.Itoa(int(user.ID))
}

func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "invalid file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fileID := uuid.New().String()
	uploaderID := h.uploader(r)

	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	if err := h.Storage.PutObject(r.Context(), fileID, file, header.Size, mime); err != nil {
		http.Error(w, "storage upload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.Queries.InsertFile(r.Context(), sqlc.InsertFileParams{
		ID:         fileID,
		Filename:   header.Filename,
		Size:       header.Size,
		MimeType:   mime,
		StorageKey: fileID,
		UploaderID: uploaderID,
		// No owning plugin: this came from a user through the browser, and
		// stays reachable by plugins that are given its id.
		OwnerPluginKey: "",
	}); err != nil {
		http.Error(w, "database insert failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"file_id":"` + fileID + `"}`))
}

// Download serves /api/system/files/download/{file_id}/{temp_token} using only
// clean path parameters (no query strings permitted).
func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not allowed on download", http.StatusBadRequest)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/system/files/download/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid request path", http.StatusBadRequest)
		return
	}
	fileID, tempToken := parts[0], parts[1]

	// 1. Verify the short-lived token is bound to this exact file.
	tokenFileID, err := h.Queries.VerifyDownloadToken(r.Context(), tempToken)
	if err != nil || tokenFileID != fileID {
		http.Error(w, "unauthorized or expired token", http.StatusUnauthorized)
		return
	}

	// 2. Load file metadata.
	fMeta, err := h.Queries.GetFile(r.Context(), fileID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// 3. Stream from object storage.
	stream, err := h.Storage.GetObject(r.Context(), fMeta.StorageKey)
	if err != nil {
		http.Error(w, "failed to retrieve file", http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", fMeta.MimeType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fMeta.Filename+"\"")
	io.Copy(w, stream)
}
