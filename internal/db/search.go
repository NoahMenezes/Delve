package db

import (
	"database/sql"
	"strings"
)

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
	// Column list + scan order come from fileColumns/scanFileRecord
	// (files.go) — one place to update when FileRecord gains a field.
	const basePrefix = "SELECT\n\t\t"
	const baseSuffix = "\n\tFROM files_fts\n\tJOIN files ON files.id = files_fts.rowid\n\tWHERE files_fts MATCH ?\n\t"
	base := basePrefix + fileColumns + baseSuffix
	withExt := base + ` AND files.extension = ? ORDER BY bm25(files_fts) LIMIT ?;`
	withoutExt := base + ` ORDER BY bm25(files_fts) LIMIT ?;`

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
		if err := scanFileRecord(rows, &rec); err != nil {
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
