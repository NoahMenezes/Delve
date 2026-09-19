package db

import (
	"database/sql"
	"path/filepath"
)

// fileColumns is the canonical 11-column files SELECT list, shared by
// SearchFiles and GetAllEmbeddings so both read FileRecord fields in
// the same order. scanFileRecord below encodes that order once: when
// FileRecord gains a field, update these two spots only instead of
// hunting down every duplicated rows.Scan.
const fileColumns = "files.path, files.name, files.extension, files.size_bytes, " +
	"files.modified_at, files.created_at, files.indexed_at, files.content_hash, " +
	"files.content, files.content_extracted_at, files.embedded_at"

// scanFileRecord scans the 11 file columns in canonical fileColumns
// order into rec. See fileColumns comment for why this lives in one
// place. Extra destinations (e.g. embeddings.dim/vector trailing the
// file columns in GetAllEmbeddings) ride along via extra so the whole
// row is still consumed by a single Scan — database/sql forbids a
// second Scan on the same row.
func scanFileRecord(rows *sql.Rows, rec *FileRecord, extra ...any) error {
	dests := []any{
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
	}
	return rows.Scan(append(dests, extra...)...)
}

// ListFiles returns every indexed file under root (including root
// itself if it's a file row), ordered by path. Powers index-wide
// features like organize suggestions that must see unembedded rows
// too — GetAllEmbeddings only covers files with vectors.
func ListFiles(database *sql.DB, root string) ([]FileRecord, error) {
	rows, err := database.Query(
		"SELECT "+fileColumns+" FROM files WHERE path = ? OR path LIKE ? ORDER BY path;",
		root, root+string(filepath.Separator)+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := scanFileRecord(rows, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

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

// DeleteFile removes one indexed path and its embedding vector, if
// any. The files_ad trigger deletes the row's FTS tokens
// automatically, so keyword search forgets the file with no extra
// work. Returns deleted=false,nil when the path was never indexed —
// the watcher logs that as "ignored", not an error, because
// create-then-delete races make it routine.
func DeleteFile(database *sql.DB, path string) (deleted bool, err error) {
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	// Embeddings reference files.id (no enforced FK — see schema.go),
	// so delete the vector first via the path's row id, then the row.
	if _, err := tx.Exec(
		`DELETE FROM embeddings WHERE file_id = (SELECT id FROM files WHERE path = ?);`,
		path,
	); err != nil {
		tx.Rollback()
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM files WHERE path = ?;`, path)
	if err != nil {
		tx.Rollback()
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetFileCount returns the total rows in `files` — a quick sanity
// check after a scan ("did anything actually get indexed?").
func GetFileCount(database *sql.DB) (int, error) {
	var count int
	// QueryRow + Scan is the database/sql idiom for a single value.
	err := database.QueryRow(`SELECT COUNT(*) FROM files;`).Scan(&count)
	return count, err
}
