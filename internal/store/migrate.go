package store

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/luongthanhtung03/qdt-test/migrations"
)

// configureGoose points goose at the embedded migration files. Calling it more
// than once is harmless, which matters because tests open many databases.
func configureGoose() error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	return goose.SetDialect("sqlite3")
}

// Migrate applies all pending migrations.
//
// It runs on the write pool so migration DDL takes the same single-writer path
// as everything else; two processes booting at once therefore serialise rather
// than colliding.
func (db *DB) Migrate(ctx context.Context) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("configure goose: %w", err)
	}
	if err := goose.UpContext(ctx, db.Write, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back the most recent migration. Used by the
// up/down/up reversibility test.
func (db *DB) MigrateDown(ctx context.Context) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("configure goose: %w", err)
	}
	if err := goose.DownContext(ctx, db.Write, "."); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// MigrationVersion reports the currently applied migration version.
func (db *DB) MigrationVersion(ctx context.Context) (int64, error) {
	if err := configureGoose(); err != nil {
		return 0, fmt.Errorf("configure goose: %w", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db.Write)
	if err != nil {
		return 0, fmt.Errorf("read migration version: %w", err)
	}
	return v, nil
}
