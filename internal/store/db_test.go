package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/store"
)

// openTestDB opens a migrated database in a temporary directory.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.Migrate(context.Background()))
	return db
}

// TestOpen_AppliesPragmas guards the connection setup described in
// docs/DESIGN.md section 4. These pragmas are the difference between a service
// that survives concurrent writes and one that returns SQLITE_BUSY under load,
// and a typo in the DSN would fail silently -- SQLite ignores pragmas it does
// not understand.
func TestOpen_AppliesPragmas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"synchronous", "1"}, // NORMAL
		{"busy_timeout", "5000"},
	} {
		t.Run(tc.pragma, func(t *testing.T) {
			var got string
			require.NoError(t, db.Write.QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&got))
			require.Equal(t, tc.want, got, "write pool: PRAGMA %s", tc.pragma)

			require.NoError(t, db.Read.QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&got))
			require.Equal(t, tc.want, got, "read pool: PRAGMA %s", tc.pragma)
		})
	}
}

// TestMigrations_UpDownUp is scenario 9 from docs/DESIGN.md section 9:
// migrations must be reversible and re-runnable.
func TestMigrations_UpDownUp(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	require.NoError(t, db.Migrate(ctx))
	v, err := db.MigrationVersion(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, v)
	requireTableExists(t, db, "contents")

	require.NoError(t, db.MigrateDown(ctx))
	v, err = db.MigrationVersion(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 0, v)
	requireTableAbsent(t, db, "contents")

	require.NoError(t, db.Migrate(ctx))
	v, err = db.MigrationVersion(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, v)
	requireTableExists(t, db, "contents")

	// Re-running an already-applied migration must be a no-op, not an error.
	require.NoError(t, db.Migrate(ctx))
}

// TestTx_RollsBackOnError proves the helper does not leave partial writes
// behind.
func TestTx_RollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sentinel := context.Canceled
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO contents (id, slug, current_version, created_at, updated_at)
			 VALUES ('id-1', 'slug-1', 1, 0, 0)`)
		require.NoError(t, err)
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	var count int
	require.NoError(t, db.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&count))
	require.Zero(t, count, "the failed transaction must not have committed a row")
}

// TestTx_RollsBackOnPanic checks the panic path, which is easy to get wrong:
// a deferred rollback that swallows the panic would turn a bug into silent
// data loss.
func TestTx_RollsBackOnPanic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	require.Panics(t, func() {
		_ = db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO contents (id, slug, current_version, created_at, updated_at)
				 VALUES ('id-1', 'slug-1', 1, 0, 0)`)
			require.NoError(t, err)
			panic("boom")
		})
	}, "the panic must propagate to the caller")

	var count int
	require.NoError(t, db.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&count))
	require.Zero(t, count, "the panicking transaction must not have committed a row")

	// The single write connection must still be usable: a leaked transaction
	// on a one-slot pool would deadlock every later write.
	require.NoError(t, db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO contents (id, slug, current_version, created_at, updated_at)
			 VALUES ('id-2', 'slug-2', 1, 0, 0)`)
		return err
	}))
}

// TestIsUniqueViolation checks the driver-code-based detection used to map
// duplicate slugs to 409.
func TestIsUniqueViolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	insert := func(id, slug string) error {
		return db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO contents (id, slug, current_version, created_at, updated_at)
				 VALUES (?, ?, 1, 0, 0)`, id, slug)
			return err
		})
	}

	require.NoError(t, insert("id-1", "same-slug"))

	err := insert("id-2", "same-slug")
	require.Error(t, err)
	require.True(t, store.IsUniqueViolation(err), "duplicate slug should be a unique violation, got %v", err)

	require.False(t, store.IsUniqueViolation(context.Canceled))
	require.False(t, store.IsUniqueViolation(nil))
}

// TestForeignKeysEnforced confirms PRAGMA foreign_keys actually took effect.
// SQLite silently ignores foreign keys when the pragma is off, so this is the
// behavioural counterpart to the pragma readback above.
func TestForeignKeysEnforced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO content_versions (content_id, version, title, body, created_at)
			 VALUES ('does-not-exist', 1, 't', 'b', 0)`)
		return err
	})
	require.Error(t, err, "insert referencing a missing content row must fail")
}

func requireTableExists(t *testing.T, db *store.DB, table string) {
	t.Helper()
	var n int
	require.NoError(t, db.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n))
	require.Equal(t, 1, n, "table %s should exist", table)
}

func requireTableAbsent(t *testing.T, db *store.DB, table string) {
	t.Helper()
	var n int
	require.NoError(t, db.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n))
	require.Zero(t, n, "table %s should have been dropped", table)
}
