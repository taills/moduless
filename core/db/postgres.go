package db

import (
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

//go:embed all:migrations
var migrationsFS embed.FS

// InitDB opens a PostgreSQL connection pool and applies all pending migrations.
// Pool limits.
//
// database/sql defaults to an unbounded pool, which means load is passed
// straight through to PostgreSQL — and PostgreSQL answers "too many clients"
// rather than queueing. At that point nothing can reach the database: not the
// plugin that caused it, not the other plugins, not Core's own session lookups.
// A bounded pool turns that into waiting, which is recoverable.
//
// Twenty-five is well under a default max_connections of 100, leaving room for
// migrations, psql sessions and a second Core during a deployment.
const (
	MaxOpenConns    = 25
	MaxIdleConns    = 10
	ConnMaxLifetime = 30 * time.Minute
)

func InitDB(connStr string) (*sql.DB, error) {
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	conn.SetMaxOpenConns(MaxOpenConns)
	conn.SetMaxIdleConns(MaxIdleConns)
	// Recycled periodically so a connection to a database that has been failed
	// over or restarted behind a proxy does not live forever.
	conn.SetConnMaxLifetime(ConnMaxLifetime)

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := RunMigrations(conn); err != nil {
		return nil, err
	}
	return conn, nil
}

// RunMigrations applies the embedded migration set to an existing pool.
func RunMigrations(conn *sql.DB) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	driver, err := postgres.WithInstance(conn, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("init migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
