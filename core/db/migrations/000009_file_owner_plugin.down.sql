DROP INDEX IF EXISTS idx_system_files_owner;
ALTER TABLE system_files DROP COLUMN IF EXISTS owner_plugin_key;
