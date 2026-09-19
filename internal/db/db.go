// Package db owns Delve's local SQLite metadata index.
//
// Phase 1 scope: pure file *metadata* only (no content, no embeddings).
// The database lives at ~/.delve/delve.db so every `delve scan` run
// shares one index — no flags or config needed yet.
package db

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
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
	// Phase 3 columns must exist before the FTS layer references them.
	if err := migrateContentColumns(database); err != nil {
		return err
	}
	// Phase 4 embedding storage. Same one-path migration pattern.
	if err := migrateEmbeddingSchema(database); err != nil {
		return err
	}
	return ensureFTSSchema(database)
}

// migrateContentColumns adds Phase 3's content columns to databases
// created by earlier phases. Fresh databases take the same path — the
// base schema above intentionally omits the new columns so there is
// exactly one code path. (SQLite has no ADD COLUMN IF NOT EXISTS, so
// we inspect PRAGMA table_info first; the whole migration is
// idempotent and a half-finished run simply completes next open.)
func migrateContentColumns(database *sql.DB) error {
	cols, err := tableColumns(database, "files")
	if err != nil {
		return err
	}
	if !cols["content"] {
		if _, err := database.Exec(`ALTER TABLE files ADD COLUMN content TEXT;`); err != nil {
			return err
		}
	}
	if !cols["content_extracted_at"] {
		if _, err := database.Exec(`ALTER TABLE files ADD COLUMN content_extracted_at INTEGER;`); err != nil {
			return err
		}
	}
	return nil
}

// migrateEmbeddingSchema adds Phase 4's vector storage. Vectors live in
// a dedicated `embeddings` table rather than a BLOB column on `files`,
// for one reason: row width. Metadata queries (counts, listings, FTS
// joins) should never drag 1.5KB of floats per row through the pager;
// the vectors are only read by the brute-force semantic scan, which
// selects from this table directly. file_id mirrors files.id 1:1
// (same integer, PRIMARY KEY on both sides) with no enforced FOREIGN
// KEY — the codebase never enables SQLite's foreign_keys pragma, so a
// declared FK would be decoration; integrity instead comes from a
// single writer path (UpsertEmbedding touches both tables in one
// transaction) plus scanner cleanup on Phase 6.
//
// VECTOR SERIALIZATION, explained: SQLite has no vector type, so a
// []float32 becomes a BLOB of concatenated little-endian IEEE-754
// bytes via encoding/binary — dim*4 bytes, 1536 for MiniLM-384.
// Little-endian is arbitrary but must be consistent between
// encodeVector and decodeVector; dim is stored alongside so a future
// model change fails loudly on read instead of silently mis-scoring.
func migrateEmbeddingSchema(database *sql.DB) error {
	cols, err := tableColumns(database, "files")
	if err != nil {
		return err
	}
	if !cols["embedded_at"] {
		if _, err := database.Exec(`ALTER TABLE files ADD COLUMN embedded_at INTEGER;`); err != nil {
			return err
		}
	}
	const table = `
	CREATE TABLE IF NOT EXISTS embeddings (
		file_id INTEGER PRIMARY KEY,
		dim     INTEGER NOT NULL,
		vector  BLOB NOT NULL
	);
	`
	_, err = database.Exec(table)
	return err
}

// tableColumns returns the current column set of a table via
// PRAGMA table_info — the standard introspection idiom for
// "does this column exist yet?" migration checks.
func tableColumns(database *sql.DB, table string) (map[string]bool, error) {
	rows, err := database.Query(`SELECT name FROM pragma_table_info(?);`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ensureFTSSchema creates the FTS5 full-text index over file names,
// paths, AND document content (Phase 3 adds content), plus the
// triggers that keep it in sync.
//
// FTS5 EXTERNAL CONTENT TABLES, explained: a normal FTS5 table stores
// its own copy of the text. With content='files', content_rowid='id',
// the FTS table instead *references* our existing `files` table — the
// indexed text lives in exactly one place. files_fts only holds the
// inverted index (token -> row); the real strings are read from
// `files` at query time. No duplication, no drift.
//
// TRIGGER SYNC, explained: SQLite does NOT auto-update an FTS index
// when the content table changes — we must do it with triggers.
// AFTER INSERT adds the new row's tokens; AFTER DELETE removes the old
// row's tokens via FTS5's special 'delete' command (that odd-looking
// INSERT with the table name as the first value is FTS5 syntax meaning
// "forget everything indexed under this rowid"); AFTER UPDATE does
// delete-then-insert because any column may have changed. From then
// on, every UpsertFile/UpdateFileContent automatically maintains the
// index — the scanner and search code never touch files_fts directly.
//
// MIGRATION, explained: you cannot ALTER a virtual table, so adding
// the content column means drop-and-recreate. We detect the old shape
// by reading the table's own CREATE statement from sqlite_master: if
// files_fts is missing or lacks a content column, we drop the three
// triggers FIRST (old bodies reference the old 3-column shape and
// would break every write the moment the table changes), drop the FTS
// table (its shadow tables go with it), recreate everything, and run
// one 'rebuild' to re-index all rows. If the shape is already current,
// triggers are ensured with IF NOT EXISTS and no rebuild runs —
// important, because InitDB opens on every command and rebuilding the
// content index on every `delve search` would be pure waste.
func ensureFTSSchema(database *sql.DB) error {
	var ftsSQL sql.NullString
	err := database.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'files_fts';`,
	).Scan(&ftsSQL)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !ftsSQL.Valid || !strings.Contains(ftsSQL.String, "content, content=") {
		return migrateFTSShape(database)
	}

	// Shape is current: self-heal missing triggers (e.g. user deleted
	// one by hand) without touching the index.
	const triggers = `
	CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;
	CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
	END;
	CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;
	`
	_, err = database.Exec(triggers)
	return err
}

// migrateFTSShape drops and recreates the FTS index with the current
// column set, then rebuilds it from `files`. Runs once per shape
// change (Phase 2 -> Phase 3); every later open takes the cheap path.
func migrateFTSShape(database *sql.DB) error {
	const migration = `
	DROP TRIGGER IF EXISTS files_ai;
	DROP TRIGGER IF EXISTS files_ad;
	DROP TRIGGER IF EXISTS files_au;
	DROP TABLE IF EXISTS files_fts;
	CREATE VIRTUAL TABLE files_fts USING fts5(
		name, path, content, content='files', content_rowid='id'
	);
	CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;
	CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
	END;
	CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;
	INSERT INTO files_fts(files_fts) VALUES('rebuild');
	`
	_, err := database.Exec(migration)
	return err
}

// UpsertFile inserts a file row, or updates it if the path already
// exists. SQLite's ON CONFLICT(path) DO UPDATE (an "upsert", needs
// SQLite 3.24+) is what makes re-scans cheap and idempotent: run the
// scan twice, still one row per file, with fresh timestamps.
//
// NOTE: content, content_hash, and content_extracted_at are
// deliberately ABSENT from the DO UPDATE clause. Metadata rescans
// (especially with --extract-content=false) must never wipe extracted
// text or its change-detection hash — wiping the hash would defeat
// skip-if-unchanged logic and force full re-extraction every scan.
// Content writes go exclusively through UpdateFileContent.
func UpsertFile(database *sql.DB, rec FileRecord) error {
	const query = `
	INSERT INTO files
		(path, name, extension, size_bytes, modified_at, created_at, indexed_at, content_hash, content, content_extracted_at)
	VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path) DO UPDATE SET
		name         = excluded.name,
		extension    = excluded.extension,
		size_bytes   = excluded.size_bytes,
		modified_at  = excluded.modified_at,
		created_at   = excluded.created_at,
		indexed_at   = excluded.indexed_at;
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
		rec.Content,
		rec.ContentExtractedAt,
	)
	return err
}

// UpdateFileContent stores (or clears, with nil) a file's extracted
// text, its content hash, and stamps the attempt time. The scanner
// calls it after every extraction attempt — success AND failure — so
// content_extracted_at always means "last tried". The UPDATE fires the
// files_au trigger, re-indexing the row's content tokens in FTS
// automatically.
//
// CONTENT_HASH, explained: since Phase 4 this holds the SHA-256 hex of
// the *extracted text* (not raw file bytes — PDFs embed timestamps and
// ids that change without the text changing). The scanner compares it
// before writing: identical hash means "content unchanged", skipping
// both the UPDATE (which would pointlessly re-index FTS) and
// re-embedding. This is the cheap change-detection Phase 1 reserved
// the column for. A nil hash (extraction failure) never matches, so
// failed files retry next scan.
func UpdateFileContent(database *sql.DB, path string, content *string, hash *string, extractedAt int64) error {
	_, err := database.Exec(
		`UPDATE files SET content = ?, content_hash = ?, content_extracted_at = ? WHERE path = ?;`,
		content, hash, extractedAt, path,
	)
	return err
}

// GetFileContentHash returns the stored text hash for path, or "" when
// the row is missing, never extracted, or last failed. The scanner
// uses it to skip unchanged files without touching them.
func GetFileContentHash(database *sql.DB, path string) string {
	var hash sql.NullString
	if err := database.QueryRow(`SELECT content_hash FROM files WHERE path = ?;`, path).Scan(&hash); err != nil {
		return ""
	}
	if !hash.Valid {
		return ""
	}
	return hash.String
}

// GetFileCount returns the total rows in `files` — a quick sanity
// check after a scan ("did anything actually get indexed?").
func GetFileCount(database *sql.DB) (int, error) {
	var count int
	// QueryRow + Scan is the database/sql idiom for a single value.
	err := database.QueryRow(`SELECT COUNT(*) FROM files;`).Scan(&count)
	return count, err
}

// SearchFiles performs a keyword search over indexed file names,
// paths, AND extracted document content (Phase 3) using the FTS5
// index. Results carry full metadata (joined back from `files`) and
// are ranked by FTS5's built-in bm25() relevance — best matches first.
// limit caps the row count; pass extension (e.g. "pdf" or ".pdf") to
// filter by file type, or "" for no filter.
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
		files.modified_at, files.created_at, files.indexed_at, files.content_hash,
		files.content, files.content_extracted_at, files.embedded_at
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
			&rec.Content,
			&rec.ContentExtractedAt,
			&rec.EmbeddedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// StoredEmbedding pairs a file's metadata with its decoded meaning
// vector — the unit of work for the brute-force semantic scan.
type StoredEmbedding struct {
	Record FileRecord
	Vector []float32
}

// UpsertEmbedding stores a file's embedding vector and stamps
// embedded_at, looked up by path (the codebase's universal key —
// callers never handle raw row ids). Both writes happen in one
// transaction so a crash can't leave a vector without its timestamp
// or vice versa. Re-embedding simply replaces the row.
func UpsertEmbedding(database *sql.DB, path string, vector []float32, embeddedAt int64) error {
	blob, err := encodeVector(vector)
	if err != nil {
		return err
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	// Rollback on any failure; committed explicitly below. The
	// named-return dance isn't needed — inline handling is clearer.
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO embeddings (file_id, dim, vector)
		SELECT id, ?, ? FROM files WHERE path = ?;`,
		len(vector), blob, path,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	_, err = tx.Exec(`UPDATE files SET embedded_at = ? WHERE path = ?;`, embeddedAt, path)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GetAllEmbeddings loads every stored vector with its file metadata
// in one JOIN — the brute-force semantic scan reads this whole set.
// Vectors decode (and dim-check) here so bad rows fail loudly at load,
// never as silent mis-scores mid-search.
func GetAllEmbeddings(database *sql.DB) ([]StoredEmbedding, error) {
	rows, err := database.Query(`
	SELECT
		files.path, files.name, files.extension, files.size_bytes,
		files.modified_at, files.created_at, files.indexed_at, files.content_hash,
		files.content, files.content_extracted_at, files.embedded_at,
		embeddings.dim, embeddings.vector
	FROM embeddings
	JOIN files ON files.id = embeddings.file_id;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredEmbedding
	for rows.Next() {
		var se StoredEmbedding
		var dim int
		var blob []byte
		if err := rows.Scan(
			&se.Record.Path,
			&se.Record.Name,
			&se.Record.Extension,
			&se.Record.SizeBytes,
			&se.Record.ModifiedAt,
			&se.Record.CreatedAt,
			&se.Record.IndexedAt,
			&se.Record.ContentHash,
			&se.Record.Content,
			&se.Record.ContentExtractedAt,
			&se.Record.EmbeddedAt,
			&dim,
			&blob,
		); err != nil {
			return nil, err
		}
		vec, err := decodeVector(blob, dim)
		if err != nil {
			return nil, fmt.Errorf("corrupt embedding for %s: %w", se.Record.Path, err)
		}
		se.Vector = vec
		out = append(out, se)
	}
	return out, rows.Err()
}

// EmbeddingStatus reports whether path already has an embedding newer
// than (or equal to) the given content timestamp — the scanner's
// freshness check for skipping unchanged files. ok=false means "embed
// it"; a nil embeddedAt also means "embed it".
func EmbeddingStatus(database *sql.DB, path string, contentTs int64) (ok bool, err error) {
	var embeddedAt sql.NullInt64
	err = database.QueryRow(`SELECT embedded_at FROM files WHERE path = ?;`, path).Scan(&embeddedAt)
	if err != nil {
		return false, err
	}
	return embeddedAt.Valid && embeddedAt.Int64 >= contentTs, nil
}

// encodeVector serializes a []float32 to little-endian bytes.
// encoding/binary (not unsafe casts) keeps this portable across
// architectures — endianness is explicit, not assumed.
func encodeVector(vec []float32) ([]byte, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("cannot store empty embedding vector")
	}
	blob := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(v))
	}
	return blob, nil
}

// decodeVector is encodeVector in reverse, with the dim cross-check
// that catches model swaps (a 768-dim row read as 384 would otherwise
// silently score garbage).
func decodeVector(blob []byte, dim int) ([]float32, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("invalid stored dimension %d", dim)
	}
	if len(blob) != 4*dim {
		return nil, fmt.Errorf("blob is %d bytes, want %d for dim %d", len(blob), 4*dim, dim)
	}
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec, nil
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
