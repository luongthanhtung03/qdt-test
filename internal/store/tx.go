package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Tx runs fn inside a write transaction on the single-writer pool.
//
// The transaction begins as BEGIN IMMEDIATE (see the _txlock DSN parameter in
// db.go), so it takes its write lock up front and never has to upgrade one
// mid-transaction. It rolls back on error and on panic, re-panicking after the
// rollback so the panic still reaches the recovery middleware.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback runs for both the error path and the panic path. Its own
		// error is deliberately dropped: on the error path the caller's error
		// is the useful one, and on the panic path the panic is.
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// ReadTx runs fn inside a read-only transaction on the reader pool. Useful when
// several queries must observe one consistent snapshot.
func (db *DB) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return nil
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
//
// This inspects the driver's numeric error code rather than matching on message
// text, so it does not break when SQLite rewords its diagnostics.
func IsUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	code := serr.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
		code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}
