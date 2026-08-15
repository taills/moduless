-- Record which plugin owns a file.
--
-- Without this, holding a file id was enough to read the file: any plugin with
-- files:read could mint a download token for any id, and ids travel — through
-- logs, events, and a plugin's own stored records. Ownership was already
-- implicit in the storage key ("plugins/<key>/<id>"), but a path string is not
-- an access control decision, so it is made explicit here.
--
-- Empty means the file has no owning plugin: it came through the user-facing
-- upload endpoint. Those stay reachable by plugins, which is the behaviour that
-- existed before this column and the case the upload endpoint is for.
ALTER TABLE system_files ADD COLUMN IF NOT EXISTS owner_plugin_key VARCHAR(100) NOT NULL DEFAULT '';

-- Backfill from the storage key, which already encodes the owner, so existing
-- files are protected rather than left unowned.
UPDATE system_files
   SET owner_plugin_key = split_part(storage_key, '/', 2)
 WHERE storage_key LIKE 'plugins/%'
   AND owner_plugin_key = '';

CREATE INDEX IF NOT EXISTS idx_system_files_owner ON system_files (owner_plugin_key);
