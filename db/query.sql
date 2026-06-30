-- name: GetExtensionVersion :one
SELECT version FROM extension_versions WHERE extension_key = $1;

-- name: UpdateExtensionVersion :exec
INSERT INTO extension_versions (extension_key, version, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (extension_key)
DO UPDATE SET version = EXCLUDED.version, updated_at = NOW();

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

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, action, extension_key, http_path, client_ip)
VALUES ($1, $2, $3, $4, $5);

-- name: ListAuditLogs :many
SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT $1 OFFSET $2;

-- name: GetUserByUsername :one
SELECT id, username, password_hash, role FROM system_users WHERE username = $1;

-- name: CountUsers :one
SELECT COUNT(*) FROM system_users;

-- name: CreateUser :exec
INSERT INTO system_users (username, password_hash, role) VALUES ($1, $2, $3);
