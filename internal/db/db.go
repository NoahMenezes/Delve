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
	"strings"

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
	if _, err := database.Exec(schema); err != nil {
		return err
	}
	return createFTSSchema(database)
}

// createFTSSchema creates the FTS5 full-text index over file names and
// paths, plus the triggers that keep it in sync. Also idempotent.
//
// FTS5 EXTERNAL CONTENT TABLES, explained: a normal FTS5 table stores
// its own copy of the text. With content='files', content_rowid='id',
// the FTS table instead *references* our existing `files` table — the
// indexed text lives in exactly one place. files_fts only holds the
// inverted index (token -> row); the real name/path strings are read
// from `files` at query time. No duplication, no drift.
//
// TRIGGER SYNC, explained: SQLite does NOT auto-update an FTS index
// when the content table changes — we must do it with triggers.
// AFTER INSERT adds the new row's tokens; AFTER DELETE removes the old
// row's tokens via FTS5's special 'delete' command (that odd-looking
// INSERT with the table name as the first value is FTS5 syntax meaning
// "forget everything indexed under this rowid"); AFTER UPDATE does
// delete-then-insert because either column may have changed. From then
// on, every UpsertFile automatically maintains the index — the scanner
// and search code never touch files_fts directly.
func createFTSSchema(database *sql.DB) error {
	const fts = `
	CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
		name, path, content='files', content_rowid='id'
	);
	CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
		INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
	END;
	CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
	END;
	CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
		INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
	END;
	`
	if _, err := database.Exec(fts); err != nil {
		return err
	}

	// Backfill: triggers only fire on writes that happen AFTER they
	// are created, so rows indexed before this migration (e.g. by a
	// Phase 1 scan) would be invisible to search. 'rebuild' re-indexes
	// every row of `files` from scratch. It is idempotent, so running
	// it on every open is safe; tables here are small (metadata only).
	_, err := database.Exec(`INSERT INTO files_fts(files_fts) VALUES('rebuild');`)
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

// SearchFiles performs a keyword search over indexed file names and
// paths using the FTS5 index. Results carry full metadata (joined back
// from `files`) and are ranked by FTS5's built-in bm25() relevance —
// best matches first. limit caps the row count; pass extension (e.g.
// "pdf" or ".pdf") to filter by file type, or "" for no filter.
//
// NOTE on the signature: the Phase 2 brief sketched
// SearchFiles(query, limit), but the handle and the --extension filter
// have to reach the SQL somehow. Following Phase 1's pattern, the
// *sql.DB is an explicit parameter, and extension rides along as a
// plain string rather than a new options struct — minimal and typed.
func SearchFiles(database *sql.DB, query string, limit int, extension string) ([]FileRecord, error) {
	if limit <= 0 {
		limit = 20 // sane default; a non-positive LIMIT would return nothing
	}
	match := toFTSMatch(query)
	if match == "" {
		return nil, nil // query was only punctuation/whitespace: no meaningful tokens
	}

	// files_fts MATCH ? does the keyword lookup against the inverted
	// index; the JOIN back to `files` fetches the stored metadata
	// (the FTS table itself holds no retrievable text in external
	// content mode). bm25(files_fts) scores lower-is-better, hence
	// ascending order.
	const base = `
	SELECT
		files.path, files.name, files.extension, files.size_bytes,
		files.modified_at, files.created_at, files.indexed_at, files.content_hash
	FROM files_fts
	JOIN files ON files.id = files_fts.rowid
	WHERE files_fts MATCH ?
	`
	const withExt = base + ` AND files.extension = ? ORDER BY bm25(files_fts) LIMIT ?;`
	const withoutExt = base + ` ORDER BY bm25(files_fts) LIMIT ?;`

	var rows *sql.Rows
	var err error
	if ext := normalizeExtension(extension); ext != "" {
		rows, err = database.Query(withExt, match, ext, limit)
	} else {
		rows, err = database.Query(withoutExt, match, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := rows.Scan(
			&rec.Path,
			&rec.Name,
			&rec.Extension,
			&rec.SizeBytes,
			&rec.ModifiedAt,
			&rec.CreatedAt,
			&rec.IndexedAt,
			&rec.ContentHash,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// toFTSMatch converts raw user input into a safe FTS5 MATCH expression.
// Raw input is dangerous: FTS5 reserves characters like `"`, `*`, `(,`
// `)` and words like OR/AND/NOT, so a filename query such as
// `say "hi"` would otherwise fail with a syntax error. The fix: split
// into whitespace-separated tokens, double any embedded quotes (the
// FTS5 escaping rule inside a quoted phrase), and wrap each token in
// double quotes. Quoted phrases are matched literally, and multiple
// phrases side-by-side mean AND — exactly the "all these words"
// semantics a file search wants.
func toFTSMatch(query string) string {
	tokens := strings.Fields(query)
	phrases := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		// Drop tokens with no word characters (pure punctuation like
		// "-" or ":") — they would become empty phrases FTS5 chokes on.
		trimmed := strings.Trim(tok, `"'*():^`)
		if trimmed == "" {
			continue
		}
		escaped := strings.ReplaceAll(tok, `"`, `""`)
		phrases = append(phrases, `"`+escaped+`"`)
	}
	return strings.Join(phrases, " ")
}

// normalizeExtension canonicalizes the --extension flag value to the
// form stored in the database: lowercase with a leading dot, e.g.
// "PDF" -> ".pdf", ".pdf" -> ".pdf". Returns "" for empty input,
// meaning "no extension filter".
func normalizeExtension(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}
