package db

import (
	"fmt"
)

// Declarative document migrations.
//
// NOT CONNECTED. Core never calls ApplyMigration: there is no manifest field
// to declare a migration in, and no version bookkeeping to decide when one
// should run. It is written and tested, and it runs only from tests.
//
// The gap it was meant to fill is real. ReconcileSchema only adds — it creates
// tables and indexes a manifest declares and never drops or rewrites anything
// — so a plugin upgraded from a version that stored `name` to one that expects
// `full_name` finds every existing document still carrying the old field, and
// Core does nothing about it. Today that is the plugin author's problem, and
// docs/plugin-development.md says so.
//
// Connecting this needs two decisions that have not been made: where a
// migration is declared, and how Core knows which ones have already run. All
// three actions here happen to be idempotent, so "run them all at every
// launch" is correct but pays for itself on every restart of a large
// collection. Left in place rather than deleted because the implementation is
// the easy half and the design is the part still owed.

// MigrationAction is a single declarative JSONB transformation.
type MigrationAction struct {
	Type       string // "rename_field", "drop_field", "set_default"
	Collection string
	From       string
	To         string
	Field      string
	Default    string
}

// Migration groups actions applied at a given extension version.
type Migration struct {
	Version string
	Actions []MigrationAction
}

// ApplyMigration runs every action in a migration within a single transaction,
// guaranteeing all-or-nothing semantics before the extension serves traffic.
func (m *CMDSManager) ApplyMigration(extKey string, mig Migration) error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	for _, action := range mig.Actions {
		table, err := tableName(extKey, action.Collection)
		if err != nil {
			return err
		}

		var stmt string
		switch action.Type {
		case "rename_field":
			if err := ValidateIdentifier(action.From); err != nil {
				return fmt.Errorf("rename_field from: %w", err)
			}
			if err := ValidateIdentifier(action.To); err != nil {
				return fmt.Errorf("rename_field to: %w", err)
			}
			stmt = fmt.Sprintf(
				`UPDATE %s SET data = (data - '%s') || jsonb_build_object('%s', data->'%s') WHERE data ? '%s';`,
				table, action.From, action.To, action.From, action.From)

		case "drop_field":
			if err := ValidateIdentifier(action.Field); err != nil {
				return fmt.Errorf("drop_field: %w", err)
			}
			stmt = fmt.Sprintf(`UPDATE %s SET data = data - '%s' WHERE data ? '%s';`,
				table, action.Field, action.Field)

		case "set_default":
			if err := ValidateIdentifier(action.Field); err != nil {
				return fmt.Errorf("set_default: %w", err)
			}
			stmt = fmt.Sprintf(
				`UPDATE %s SET data = jsonb_set(data, '{%s}', to_jsonb($1::text)) WHERE NOT (data ? '%s');`,
				table, action.Field, action.Field)
			if _, err := tx.Exec(stmt, action.Default); err != nil {
				return fmt.Errorf("apply set_default on %s: %w", table, err)
			}
			continue

		default:
			return fmt.Errorf("unknown migration action type %q", action.Type)
		}

		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("apply %s on %s: %w", action.Type, table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
