-- One-time backfill: any extension that still uses the legacy menu_icon /
-- menu_path fields gets a one-node menus[] tree built from them. The path
-- /icon are preserved; title falls back to display_name; entry is the
-- canonical micro-frontend html path; children is empty.
UPDATE extensions
SET menus = jsonb_build_array(jsonb_build_object(
        'path',     menu_path,
        'title',    display_name,
        'icon',     menu_icon,
        'order',    0,
        'entry',    '/extensions/' || key || '/',
        'roles',    '[]'::jsonb,
        'children', '[]'::jsonb
    ))
WHERE menus = '[]'::jsonb
  AND menu_path <> '';