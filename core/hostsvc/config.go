package hostsvc

import (
	"context"
	"database/sql"
	"fmt"
)

// DBConfig stores admin-managed plugin settings in PostgreSQL.
//
// The in-memory StaticConfig it replaces is still used where there is no
// database — Core runs without one — but it cannot outlive the process, which
// makes it useless for anything an operator sets deliberately.
type DBConfig struct {
	conn *sql.DB
}

func NewDBConfig(conn *sql.DB) *DBConfig { return &DBConfig{conn: conn} }

// Get returns everything stored for a plugin. Manifest defaults are applied
// above this, by the manager, so what a plugin ultimately sees is the declared
// defaults with these values on top.
func (c *DBConfig) Get(ctx context.Context, pluginKey string) (map[string]string, error) {
	rows, err := c.conn.QueryContext(ctx,
		`SELECT config_key, value FROM plugin_config WHERE plugin_key = $1`, pluginKey)
	if err != nil {
		return nil, fmt.Errorf("read config for %s: %w", pluginKey, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config row: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Set replaces a plugin's settings with exactly what is given.
//
// A whole-map replace rather than a per-key update, because that is what the
// console sends: the operator edits a form and saves it, and a key they
// deleted has to actually go away. Doing it in one transaction means a reader
// never catches the plugin midway between two configurations.
func (c *DBConfig) Set(ctx context.Context, pluginKey string, values map[string]string) error {
	tx, err := c.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin config write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM plugin_config WHERE plugin_key = $1`, pluginKey); err != nil {
		return fmt.Errorf("clear config for %s: %w", pluginKey, err)
	}
	for k, v := range values {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_config (plugin_key, config_key, value) VALUES ($1, $2, $3)`,
			pluginKey, k, v); err != nil {
			return fmt.Errorf("write config %s/%s: %w", pluginKey, k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit config write: %w", err)
	}
	return nil
}

// ConfigFunc adapts a function to ConfigBackend.
//
// It exists so Core can serve a plugin's configuration from the same place
// that launches it, rather than from a second store that is free to disagree.
type ConfigFunc func(ctx context.Context, pluginKey string) (map[string]string, error)

func (f ConfigFunc) Get(ctx context.Context, pluginKey string) (map[string]string, error) {
	return f(ctx, pluginKey)
}
