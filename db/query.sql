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

-- name: UpsertPendingExtension :one
INSERT INTO extensions (key, display_name, version, menu_icon, menu_path, status, updated_at)
VALUES ($1, $2, $3, $4, $5, 'pending', NOW())
ON CONFLICT (key) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        version      = EXCLUDED.version,
        menu_icon    = EXCLUDED.menu_icon,
        menu_path    = EXCLUDED.menu_path,
        updated_at   = NOW()
RETURNING *;

-- name: GetExtension :one
SELECT * FROM extensions WHERE key = $1;

-- name: ListExtensions :many
SELECT * FROM extensions ORDER BY created_at DESC;

-- name: SetExtensionStatus :exec
UPDATE extensions
SET status = @status::text,
    approved_at = CASE WHEN @status::text = 'approved' THEN NOW() ELSE approved_at END,
    updated_at = NOW()
WHERE key = @key;

-- name: DeleteExtension :exec
DELETE FROM extensions WHERE key = $1;

-- name: CreateExtensionSecret :one
INSERT INTO extension_secrets (extension_key, secret_hash, label)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListActiveExtensionSecrets :many
SELECT * FROM extension_secrets
WHERE extension_key = $1 AND revoked_at IS NULL
ORDER BY id;

-- name: ListExtensionSecrets :many
SELECT * FROM extension_secrets
WHERE extension_key = $1
ORDER BY id;

-- name: TouchExtensionSecret :exec
UPDATE extension_secrets SET last_used_at = NOW() WHERE id = $1;

-- name: RevokeExtensionSecret :exec
UPDATE extension_secrets SET revoked_at = NOW() WHERE id = $1 AND extension_key = $2;
