# Core File Service (RustFS Integration) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the Core File Service integrating RustFS (S3-compatible object storage), providing centralized file uploads, secure temporary token-based downloads via clean path URLs, and inner gRPC APIs for extensions to manage files.

**Architecture:** Core connects to RustFS via S3 SDK. Files are stored in a centralized bucket. Core HTTP gateway exposes `/api/system/files/upload` and `/api/system/files/download/<file_id>/<temp_token>` (pure path params, no query strings). Extension backend requests short-lived tokens from Core via gRPC to grant download access.

**Tech Stack:** Go, RustFS (S3 SDK), PostgreSQL 18, sqlc, go-migrate.

## Global Constraints

- No S3 credentials or direct storage connections in extensions.
- Downloads must use path parameters: `/api/system/files/download/{file_id}/{temp_token}` with zero query parameters.
- Files are saved as temporary until bound to a business resource.

---

### Task 1: RustFS S3 Storage Client & Migrations

Create PostgreSQL migrations for file metadata, and implement the RustFS storage manager wrapper in Core.

**Files:**
- Create: `db/migrations/000002_create_files.up.sql`
- Create: `db/migrations/000002_create_files.down.sql`
- Create: `core/storage/rustfs.go`
- Modify: `db/query.sql`

**Interfaces:**
- Produces: `storage.NewRustFSClient(endpoint, bucket, accessKey, secretKey) (*RustFSClient, error)`
  - `client.PutObject(ctx, fileID string, reader io.Reader, size int64, mime string) error`
  - `client.GetObject(ctx, fileID string) (io.ReadCloser, error)`

- [ ] **Step 1: Write Migration file for file registry**

Create `db/migrations/000002_create_files.up.sql`:
```sql
CREATE TABLE IF NOT EXISTS system_files (
    id VARCHAR(100) PRIMARY KEY,
    filename VARCHAR(255) NOT NULL,
    size BIGINT NOT NULL,
    mime_type VARCHAR(100) NOT NULL,
    storage_key VARCHAR(255) NOT NULL,
    uploader_id VARCHAR(100) NOT NULL,
    status VARCHAR(20) DEFAULT 'temporary', -- 'temporary', 'bound'
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS file_download_tokens (
    token VARCHAR(255) PRIMARY KEY,
    file_id VARCHAR(100) NOT NULL,
    user_id VARCHAR(100) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
```

Create `db/migrations/000002_create_files.down.sql`:
```sql
DROP TABLE IF EXISTS file_download_tokens;
DROP TABLE IF EXISTS system_files;
```

- [ ] **Step 2: Add SQL queries in `db/query.sql`**

```sql
-- name: InsertFile :exec
INSERT INTO system_files (id, filename, size, mime_type, storage_key, uploader_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetFile :one
SELECT * FROM system_files WHERE id = $1;

-- name: InsertDownloadToken :exec
INSERT INTO file_download_tokens (token, file_id, user_id, expires_at)
VALUES ($1, $2, $3, $4);

-- name: VerifyDownloadToken :one
SELECT file_id FROM file_download_tokens 
WHERE token = $1 AND expires_at > NOW();
```

- [ ] **Step 3: Write `core/storage/rustfs.go` client wrapper**

```go
package storage

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type RustFSClient struct {
	client *s3.Client
	bucket string
}

func NewRustFSClient(endpoint, bucket, accessKey, secretKey string) (*RustFSClient, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: endpoint}, nil
			})),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, err
	}

	return &RustFSClient{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
	}, nil
}

func (c *RustFSClient) PutObject(ctx context.Context, key string, reader io.Reader, size int64, mime string) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          reader,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(mime),
	})
	return err
}

func (c *RustFSClient) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}
```

- [ ] **Step 4: Compile sqlc and commit**

Run: `sqlc generate`
Run: `git add db/ core/storage/ && git commit -m "feat: implement S3 RustFS storage client and schema"`

---

### Task 2: Core HTTP Gateway Upload & Path Param Download

Implement the public upload and path-parameter based download HTTP routes on the Core gateway.

**Files:**
- Create: `core/gateway/file_handler.go`
- Modify: `core/gateway/router.go`

**Interfaces:**
- Consumes: `RustFSClient` from `core/storage`
- Produces: HTTP API Endpoints:
  - `POST /api/system/files/upload`
  - `GET /api/system/files/download/{file_id}/{temp_token}`

- [ ] **Step 1: Write `core/gateway/file_handler.go`**

```go
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ty-lab/go-web-module/core/storage"
	db "github.com/ty-lab/go-web-module/core/db/sqlc"
)

type FileHandler struct {
	Storage *storage.RustFSClient
	Queries *db.Queries
}

func NewFileHandler(s *storage.RustFSClient, q *db.Queries) *FileHandler {
	return &FileHandler{Storage: s, Queries: q}
}

func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "invalid file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fileID := uuid.New().String()
	uploaderID := r.Header.Get("X-User-Id")
	if uploaderID == "" {
		uploaderID = "anonymous"
	}

	err = h.Storage.PutObject(r.Context(), fileID, file, header.Size, header.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "storage upload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = h.Queries.InsertFile(r.Context(), db.InsertFileParams{
		ID:         fileID,
		Filename:   header.Filename,
		Size:       header.Size,
		MimeType:   header.Header.Get("Content-Type"),
		StorageKey: fileID,
		UploaderID: uploaderID,
	})
	if err != nil {
		http.Error(w, "database insert failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"file_id":"` + fileID + `"}`))
}

func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	// Parse path /api/system/files/download/{file_id}/{temp_token}
	path := strings.TrimPrefix(r.URL.Path, "/api/system/files/download/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid request path", http.StatusBadRequest)
		return
	}
	fileID := parts[0]
	tempToken := parts[1]

	// 1. Verify token
	tokenFileID, err := h.Queries.VerifyDownloadToken(r.Context(), tempToken)
	if err != nil || tokenFileID != fileID {
		http.Error(w, "unauthorized or expired token", http.StatusUnauthorized)
		return
	}

	// 2. Fetch file metadata
	fMeta, err := h.Queries.GetFile(r.Context(), fileID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// 3. Stream from S3
	stream, err := h.Storage.GetObject(r.Context(), fMeta.StorageKey)
	if err != nil {
		http.Error(w, "failed to retrieve file", http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", fMeta.MimeType)
	w.Header().Set("Content-Disposition", "attachment; filename="+fMeta.Filename)
	io.Copy(w, stream)
}
```

- [ ] **Step 2: Commit**

```bash
git add core/gateway/file_handler.go
git commit -m "feat: implement Core file upload and path param download handlers"
```

---

### Task 3: Core Inner gRPC File Service & Go SDK Client

Expose the S3 file token generation APIs to extensions via the Go SDK.

**Files:**
- Create: `core/tunnel/file_server.go`
- Create: `sdk/go/file.go`

**Interfaces:**
- Produces: SDK client functions:
  - `sdk.Files.GenerateDownloadURL(ctx, fileID, expiry) (string, error)`
  - `sdk.Files.GetMetadata(ctx, fileID) (*FileMeta, error)`

- [ ] **Step 1: Write `core/tunnel/file_server.go` gRPC handlers**

```go
package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	db "github.com/ty-lab/go-web-module/core/db/sqlc"
	pb "github.com/ty-lab/go-web-module/proto/tunnel"
)

type FileServer struct {
	pb.UnimplementedFileServiceServer
	Queries *db.Queries
}

func (s *FileServer) GenerateDownloadToken(ctx context.Context, req *pb.TokenRequest) (*pb.TokenResponse, error) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)

	expiresAt := time.Now().Add(time.Duration(req.ExpirySeconds) * time.Second)

	err := s.Queries.InsertDownloadToken(ctx, db.InsertDownloadTokenParams{
		Token:     token,
		FileID:    req.FileId,
		UserID:    req.UserId,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}

	return &pb.TokenResponse{
		Token: token,
		Url:   "/api/system/files/download/" + req.FileId + "/" + token,
	}, nil
}
```

- [ ] **Step 2: Write Go SDK Client helper in `sdk/go/file.go`**

```go
package sdk

import (
	"context"

	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"google.golang.org/grpc"
)

type FilesClient struct {
	client pb.FileServiceClient
}

func NewFilesClient(conn *grpc.ClientConn) *FilesClient {
	return &FilesClient{client: pb.NewFileServiceClient(conn)}
}

func (c *FilesClient) GenerateDownloadURL(ctx context.Context, fileID, userID string, expirySeconds int32) (string, error) {
	resp, err := c.client.GenerateDownloadToken(ctx, &pb.TokenRequest{
		FileId:        fileID,
		UserId:        userID,
		ExpirySeconds: expirySeconds,
	})
	if err != nil {
		return "", err
	}
	return resp.Url, nil
}
```

- [ ] **Step 3: Commit**

```bash
git add core/tunnel/file_server.go sdk/go/file.go
git commit -m "feat: implement FileService gRPC server and SDK clients"
```
