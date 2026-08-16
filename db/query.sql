-- name: InsertFile :exec
INSERT INTO system_files (id, filename, size, mime_type, storage_key, uploader_id, owner_plugin_key)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetFileOwner :one
SELECT owner_plugin_key FROM system_files WHERE id = $1;

-- name: GetFile :one
SELECT * FROM system_files WHERE id = $1;

-- name: InsertDownloadToken :exec
INSERT INTO file_download_tokens (token, file_id, user_id, expires_at)
VALUES ($1, $2, $3, $4);

-- name: VerifyDownloadToken :one
SELECT file_id FROM file_download_tokens
WHERE token = $1 AND expires_at > NOW();

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, action, extension_key, http_path, client_ip, trace_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListAuditLogs :many
SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT $1 OFFSET $2;

-- name: GetUserByUsername :one
SELECT id, username, password_hash, role FROM system_users WHERE username = $1;

-- name: CountUsers :one
SELECT COUNT(*) FROM system_users;

-- name: CreateUser :exec
INSERT INTO system_users (username, password_hash, role) VALUES ($1, $2, $3);

-- name: ListUsers :many
SELECT id, username, role, created_at FROM system_users ORDER BY id;

-- name: GetUserByID :one
SELECT id, username, password_hash, role, created_at FROM system_users WHERE id = $1;

-- name: UpdateUserPassword :exec
UPDATE system_users SET password_hash = $2 WHERE id = $1;

-- name: UpdateUserRole :exec
UPDATE system_users SET role = $2 WHERE id = $1;

-- name: DeleteUser :exec
DELETE FROM system_users WHERE id = $1;

-- name: CountAdmins :one
SELECT COUNT(*) FROM system_users WHERE role = 'admin';
