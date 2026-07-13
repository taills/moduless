-- menus is the canonical per-extension menu tree, replacing the legacy
-- menu_icon / menu_path single-menu pair. It is a JSONB array of MenuItem
-- objects, where each node has path/title/icon/order/entry/roles/children.
-- The legacy columns are kept untouched for backwards compatibility (Core
-- still reads them as a fallback when menus is empty).
ALTER TABLE extensions
    ADD COLUMN menus JSONB NOT NULL DEFAULT '[]'::jsonb;