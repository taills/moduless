-- Backfill is data-only; no schema is changed by 000006. The "down" of a
-- data migration is to leave the data alone — operators can drop the menus
-- they want to roll back via UPDATE if they truly need to.
SELECT 1;