// Package db owns Delve's local SQLite metadata index.
//
// Phase 1 scope: pure file *metadata* only (no content, no embeddings).
// The database lives at ~/.delve/delve.db so every `delve scan` run
// shares one index — no flags or config needed yet.
package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	// Blank import: registers the "sqlite3" SQL driver via init().
	// This is the standard database/sql idiom — we never call the
	// package directly, we just need its side effect.
	//
	// NOTE: FTS5 (needed for `delve search`) is only compiled into
	// this driver with the fts5 build tag. Every build, run, and
	// test of Delve must use it: go build -tags fts5 .
	_ "github.com/mattn/go-sqlite3"
)

// errFTS5Missing is returned when Delve was built without -tags fts5.
var errFTS5Missing = errors.New(
	"SQLite FTS5 extension not available in this build; " +
		"rebuild Delve with FTS5 enabled: go build -tags fts5 .",
)

// FileRecord is one row of the `files` table: metadata for a single
// file discovered by the scanner. All times are Unix seconds (int64)
// so they store portably as SQLite INTEGERs and avoid timezone pain.
//
// ContentHash is *string (nullable) rather than string: nil means
// "not computed yet" (never extracted or last extraction failed),
// distinct from "" meaning "computed and empty". Since Phase 4 it
// holds the SHA-256 hex of the *extracted text* for change detection
// (skip-if-unchanged in the scanner) without re-reading the file.
type FileRecord struct {
	Path        string  // absolute path; UNIQUE lookup key
	Name        string  // base name, e.g. "notes.md"
	Extension   string  // lowercase, with dot, e.g. ".md"; "" if none
	SizeBytes   int64   // file size in bytes
	ModifiedAt  int64   // mtime from the filesystem, Unix seconds
	CreatedAt   int64   // birth time if known, else falls back to mtime
	IndexedAt   int64   // when Delve last saw this file, Unix seconds
	ContentHash *string // SHA-256 hex of extracted text (Phase 4+) for change detection; nil = not computed/failed
	// Content is the extracted document text (Phase 3+), nil when none
	// is stored. ContentExtractedAt is the Unix time of the last
	// extraction *attempt*, and the two NULLs together encode state:
	//   nil, nil + supported extension  = not yet tried
	//   timestamp + nil content         = tried and failed (don't retry blindly)
	//   timestamp + text                = extracted successfully
	//   nil, nil + unsupported extension = not applicable (by definition)
	Content            *string
	ContentExtractedAt *int64
	// EmbeddedAt is the Unix time the file's content was last embedded
	// (Phase 4+), nil when never embedded. Lives on `files` (not the
	// embeddings table) so "what still needs embedding?" is answerable
	// without touching any vector BLOBs — same pattern as
	// ContentExtractedAt.
	EmbeddedAt *int64
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

	// Guard: FTS5 is compile-time optional in mattn/go-sqlite3 — it is
	// only present when built with `-tags fts5`. Without it, the
	// CREATE VIRTUAL TABLE below would fail with a cryptic
	// "no such module: fts5", so fail fast with a helpful message.
	if err := requireFTS5(database); err != nil {
		database.Close()
		return nil, err
	}

	if err := createSchema(database); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// requireFTS5 reports whether this build's bundled SQLite has the
// FTS5 extension compiled in. Always build Delve with:
//
//	go build -tags fts5 .
func requireFTS5(database *sql.DB) error {
	var enabled int
	err := database.QueryRow(`SELECT sqlite_compileoption_used('ENABLE_FTS5');`).Scan(&enabled)
	if err != nil {
		return err
	}
	if enabled != 1 {
		return errFTS5Missing
	}
	return nil
}
