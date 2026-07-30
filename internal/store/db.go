// Package store owns database access: connection setup, the transaction
// helper, and the hand-written SQL for each table.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite" // pure-Go driver; registers itself as "sqlite"
)

// DB holds two connection pools over the same SQLite file.
//
// SQLite allows exactly one writer at a time regardless of what the client
// does. Capping the write pool at a single connection makes Go's connection
// pool the queue, which is strictly better than discovering the limit as a
// SQLITE_BUSY error partway through a transaction. Readers are unaffected:
// under WAL they run concurrently with the writer.
//
// See docs/DESIGN.md section 4.
type DB struct {
	// Write is capped at one connection and opens transactions with
	// BEGIN IMMEDIATE. Use it for anything that mutates.
	Write *sql.DB
	// Read is a wider pool for queries. Never write through it.
	Read *sql.DB

	path string
}

// pragmas are applied to every connection in both pools.
//
// modernc.org/sqlite uses the _pragma=name(value) DSN form; mattn/go-sqlite3
// would spell these _journal_mode=WAL and friends.
const pragmas = "_pragma=journal_mode(WAL)" + // concurrent readers alongside one writer
	"&_pragma=busy_timeout(5000)" + // wait on contention instead of failing instantly
	"&_pragma=foreign_keys(1)" + // SQLite defaults this OFF
	"&_pragma=synchronous(NORMAL)" // safe under WAL, much faster than FULL

// Open creates both pools for the SQLite database at path, creating the parent
// directory if needed.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	// The path becomes part of a URI, so it has to be escaped. On Windows this
	// also normalises the backslashes that would otherwise be read as escapes.
	base := "file:" + url.PathEscape(filepath.ToSlash(path)) + "?" + pragmas

	// _txlock=immediate makes every transaction on this pool start as
	// BEGIN IMMEDIATE. Without it, a transaction that reads before it writes
	// has to upgrade its lock, and a failed upgrade is an immediate
	// unrecoverable SQLITE_BUSY that busy_timeout does not retry.
	writeDB, err := sql.Open("sqlite", base+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	readDB, err := sql.Open("sqlite", base)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	readers := runtime.NumCPU()
	if readers < 4 {
		readers = 4
	}
	readDB.SetMaxOpenConns(readers)
	readDB.SetMaxIdleConns(readers)

	db := &DB{Write: writeDB, Read: readDB, path: path}

	// Force a real connection now so a bad path or unwritable directory fails
	// at boot rather than on the first request. The write pool goes first: it
	// creates the file and performs the one-time WAL switch.
	if err := writeDB.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	if err := readDB.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect read pool to %s: %w", path, err)
	}
	return db, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// Close shuts down both pools, returning the first error encountered.
func (db *DB) Close() error {
	var firstErr error
	if db.Read != nil {
		if err := db.Read.Close(); err != nil {
			firstErr = err
		}
	}
	if db.Write != nil {
		if err := db.Write.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
