package db

import (
	"database/sql"
	"strings"
)

// FTS trigger bodies, shared by ensureFTSSchema and migrateFTSShape.
// Stored WITHOUT the "CREATE TRIGGER [IF NOT EXISTS] <name>" prefix
// because the two call sites differ only in that prefix (self-heal
// uses IF NOT EXISTS, migration uses plain CREATE after DROP); the
// AFTER...END body is identical, so it lives here exactly once.
const trigFilesAI = `AFTER INSERT ON files BEGIN
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;`

const trigFilesAD = `AFTER DELETE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
	END;`

const trigFilesAU = `AFTER UPDATE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, name, path, content) VALUES ('delete', old.id, old.name, old.path, old.content);
		INSERT INTO files_fts(rowid, name, path, content) VALUES (new.id, new.name, new.path, new.content);
	END;`

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
	_, err = database.Exec(
		"CREATE TRIGGER IF NOT EXISTS files_ai " + trigFilesAI + "\n" +
			"CREATE TRIGGER IF NOT EXISTS files_ad " + trigFilesAD + "\n" +
			"CREATE TRIGGER IF NOT EXISTS files_au " + trigFilesAU,
	)
	return err
}

// migrateFTSShape drops and recreates the FTS index with the current
// column set, then rebuilds it from `files`. Runs once per shape
// change (Phase 2 -> Phase 3); every later open takes the cheap path.
func migrateFTSShape(database *sql.DB) error {
	_, err := database.Exec(
		`DROP TRIGGER IF EXISTS files_ai;
	DROP TRIGGER IF EXISTS files_ad;
	DROP TRIGGER IF EXISTS files_au;
	DROP TABLE IF EXISTS files_fts;
	CREATE VIRTUAL TABLE files_fts USING fts5(
		name, path, content, content='files', content_rowid='id'
	);` + "\nCREATE TRIGGER files_ai " + trigFilesAI +
			"\nCREATE TRIGGER files_ad " + trigFilesAD +
			"\nCREATE TRIGGER files_au " + trigFilesAU +
			"\nINSERT INTO files_fts(files_fts) VALUES('rebuild');",
	)
	return err
}
