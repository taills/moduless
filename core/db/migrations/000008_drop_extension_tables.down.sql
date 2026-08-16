-- This one is a one-way door, and says so rather than letting the rollback
-- walk past it into confusion.
--
-- Recreating these tables would produce empty shells: the extension model they
-- served no longer exists in the code, so there is nothing to roll back to.
-- Restore from a backup taken before the migration instead.
--
-- The no-op this used to be was worse than useless. `migrate down` does not
-- stop at a migration that declines to do anything — it carries on, and the
-- next one along tries to ALTER a table this one dropped. What an operator saw
-- was `relation "extensions" does not exist` from migration 000005, three
-- steps away from the decision that caused it.
DO $$
BEGIN
    RAISE EXCEPTION 'migration 000008 cannot be rolled back: it dropped the '
        'extension tables, and recreating them would give you empty shells '
        'rather than your data. Roll back to version 8 to undo everything '
        'after it; to go further, restore from a backup taken before this '
        'migration ran.';
END
$$;
