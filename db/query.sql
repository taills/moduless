-- name: GetExtensionVersion :one
SELECT version FROM extension_versions WHERE extension_key = $1;

-- name: UpdateExtensionVersion :exec
INSERT INTO extension_versions (extension_key, version, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (extension_key)
DO UPDATE SET version = EXCLUDED.version, updated_at = NOW();
