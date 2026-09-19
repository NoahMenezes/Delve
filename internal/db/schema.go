package db

import (
	"database/sql"
)

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
