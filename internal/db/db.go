// Package db owns Delve's local SQLite metadata index.
//
// Phase 1 scope: pure file *metadata* only (no content, no embeddings).
// The database lives at ~/.delve/delve.db so every `delve scan` run
// shares one index — no flags or config needed yet.
package db

import (
	"database/sql"
	"os"
	"path/filepath"

	// Blank import: registers the "sqlite3" SQL driver via init().
	// This is the standard database/sql idiom — we never call the
	// package directly, we just need its side effect.
	_ "github.com/mattn/go-sqlite3"
)

// FileRecord is one row of the `files` table: metadata for a single
// file discovered by the scanner. All times are Unix seconds (int64)
// so they store portably as SQLite INTEGERs and avoid timezone pain.
//
// ContentHash is *string (nullable) rather than string: nil means
// "not computed yet" (all of Phase 1), distinct from "" meaning
// "computed and empty". It becomes useful in later phases for
// detecting content changes without re-reading the file.
type FileRecord struct {
	Path        string  // absolute path; UNIQUE lookup key
	Name        string  // base name, e.g. "notes.md"
	Extension   string  // lowercase, with dot, e.g. ".md"; "" if none
	SizeBytes   int64   // file size in bytes
	ModifiedAt  int64   // mtime from the filesystem, Unix seconds
	CreatedAt   int64   // birth time if known, else falls back to mtime
	IndexedAt   int64   // when Delve last saw this file, Unix seconds
	ContentHash *string // nil in Phase 1; reserved for later phases
}

// DefaultDBPath returns ~/.delve/delve.db.
// os.UserHomeDir is the portable way to find $HOME (it handles
// Windows differently from Unix under the hood).
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// No portable home (very rare) — fall back to a local file
		// so Delve still works instead of crashing.
		return filepath.Join(".", "delve.db")
	}
	return filepath.Join(home, ".delve", "delve.db")
}

// InitDB opens (creating parent dirs + file as needed) the SQLite
// database at the default location and ensures the schema exists.
// It returns the open *sql.DB; the caller owns it and must Close it.
//
// Creating the directory first matters: the sqlite driver will create
// the .db file, but NOT missing parent directories.
func InitDB() (*sql.DB, error) {
	path := DefaultDBPath()

	// MkdirAll is a no-op if ~/.delve already exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	// database/sql opens lazily — it doesn't touch disk until the
	// first query, so we Ping to fail fast on a bad path/permissions.
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, err
	}

	if err := createSchema(database); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// createSchema is idempotent (IF NOT EXISTS) so re-scans and upgrades
// never wipe existing data.
func createSchema(database *sql.DB) error {
	// path is UNIQUE: re-scanning the same file must update the row,
	// not insert a duplicate. UNIQUE also auto-indexes the column,
	// but we add an explicit index below anyway per the Phase 1 spec
	// to make the "lookup by path" intent obvious.
	const schema = `
	CREATE TABLE IF NOT EXISTS files (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		path        TEXT NOT NULL UNIQUE,
		name        TEXT NOT NULL,
		extension   TEXT NOT NULL,
		size_bytes  INTEGER NOT NULL,
		modified_at INTEGER NOT NULL,
		created_at  INTEGER NOT NULL,
		indexed_at  INTEGER NOT NULL,
		content_hash TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
	`
	_, err := database.Exec(schema)
	return err
}

// UpsertFile inserts a file row, or updates it if the path already
// exists. SQLite's ON CONFLICT(path) DO UPDATE (an "upsert", needs
// SQLite 3.24+) is what makes re-scans cheap and idempotent: run the
// scan twice, still one row per file, with fresh timestamps.
func UpsertFile(database *sql.DB, rec FileRecord) error {
	const query = `
	INSERT INTO files
		(path, name, extension, size_bytes, modified_at, created_at, indexed_at, content_hash)
	VALUES
		(?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path) DO UPDATE SET
		name         = excluded.name,
		extension    = excluded.extension,
		size_bytes   = excluded.size_bytes,
		modified_at  = excluded.modified_at,
		created_at   = excluded.created_at,
		indexed_at   = excluded.indexed_at,
		content_hash = excluded.content_hash;
	`
	// database/sql uses ? placeholders for sqlite (not $1 like Postgres).
	_, err := database.Exec(
		query,
		rec.Path,
		rec.Name,
		rec.Extension,
		rec.SizeBytes,
		rec.ModifiedAt,
		rec.CreatedAt,
		rec.IndexedAt,
		rec.ContentHash, // nil *string -> SQL NULL, exactly what we want
	)
	return err
}

// GetFileCount returns the total rows in `files` — a quick sanity
// check after a scan ("did anything actually get indexed?").
func GetFileCount(database *sql.DB) (int, error) {
	var count int
	// QueryRow + Scan is the database/sql idiom for a single value.
	err := database.QueryRow(`SELECT COUNT(*) FROM files;`).Scan(&count)
	return count, err
}
