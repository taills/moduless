package db

import (
	"fmt"
)

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
