package db

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

//go:embed all:migrations
var migrationsFS embed.FS

// InitDB opens a PostgreSQL connection pool and applies all pending migrations.
func InitDB(connStr string) (*sql.DB, error) {
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
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
