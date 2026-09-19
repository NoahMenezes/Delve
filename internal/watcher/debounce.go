// Debouncing for the live watcher: collapsing rapid-fire fsnotify
// events per path into one pipeline run.
package watcher

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/scanner"
)

// debounceDelay is how long to wait for a path to go quiet before
// processing it. Editors often write a file several times in quick
// succession (write temp + rename, or multiple Write events per
// save); without debouncing each burst would trigger redundant
// re-extraction and re-embedding — the slowest steps in the pipeline.
const debounceDelay = 500 * time.Millisecond

// pathDebouncer collapses rapid-fire events per path into one
// processing pass after 500ms of quiet, and serializes all database
// work behind processMu (SQLite tolerates concurrent reads but
// concurrent writes hit SQLITE_BUSY — one file at a time avoids it).
type pathDebouncer struct {
	notify   *fsnotify.Watcher
	database *sql.DB
	opts     scanner.ScanOptions
	logf     func(string, ...any)

	mu        sync.Mutex // guards timers
	timers    map[string]*time.Timer
	processMu sync.Mutex // serializes ScanFile/DeleteFile
}

// schedule (re)starts the quiet window for path. Every new event
// pushes processing 500ms out; only the last event in a burst pays
// for extraction + embedding.
func (w *pathDebouncer) schedule(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[path]; ok {
		t.Stop()
	}
	w.timers[path] = time.AfterFunc(debounceDelay, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()
		w.process(path)
	})
}

// stopAll drops pending paths on shutdown (see Watch's ctx.Done note).
func (w *pathDebouncer) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, t := range w.timers {
		t.Stop()
		delete(w.timers, path)
	}
}

// process handles one quiet path: Stat decides what happened, then
// the shared scan/delete pipeline runs. Rename needs no special
// case: fsnotify delivers it as Rename on the old path (Stat fails
// here → delete) plus Create on the new path (scheduled separately
// → index). Remove + create, exactly the Phase 6 spec.
func (w *pathDebouncer) process(path string) {
	w.processMu.Lock()
	defer w.processMu.Unlock()

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			w.handleRemove(path)
			return
		}
		w.logf("warning: could not stat %s: %v", path, err)
		return
	}
	if info.IsDir() {
		w.processDir(path)
		return
	}
	// Symlinks and non-regular files (sockets, devices) are never
	// indexed — ScanFile skips them silently, so stay silent here
	// too instead of logging a phantom "indexed".
	indexed, err := scanner.ScanFile(w.database, path, w.opts)
	if err != nil {
		// Race: deleted between schedule and process. Fall back
		// to removal so the row doesn't go stale.
		if os.IsNotExist(err) {
			w.handleRemove(path)
			return
		}
		w.logf("warning: could not index %s: %v", path, err)
		return
	}
	if indexed {
		abs, _ := filepath.Abs(path)
		if abs == "" {
			abs = path
		}
		w.logf("indexed: %s", abs)
	}
}

// processDir Adds a newly appeared directory (and any subdirs) to
// the watch, then indexes files already inside it. The second step
// matters for `mv bigdir watched/` or `mkdir -p a/b`: files that
// existed before the Add generate no Create events of their own, so
// without this walk they would stay unindexed until the next full
// scan. Double-scans (dir walk + the file's own Create event) are
// harmless: upserts are idempotent and unchanged content skips
// extraction/embedding via the stored hash.
func (w *pathDebouncer) processDir(path string) {
	if scanner.ShouldSkipDir(filepath.Base(path)) {
		return
	}
	if err := addRecursive(w.notify, path); err != nil {
		w.logf("warning: could not watch %s: %v", path, err)
	}
	_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if p != path && scanner.ShouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		indexed, err := scanner.ScanFile(w.database, p, w.opts)
		if err != nil {
			return nil
		}
		if indexed {
			abs, _ := filepath.Abs(p)
			if abs == "" {
				abs = p
			}
			w.logf("indexed: %s", abs)
		}
		return nil
	})
}

// handleRemove deletes the indexed row for path, plus any rows under
// it when a whole directory vanished. ListFiles(path) returns the
// path itself when it was a file, or every child when it was a dir —
// one code path covers both. The FTS triggers clean the search index
// per deleted row automatically. Paths never indexed (temp files,
// editor swap files) log nothing: create-then-delete races make
// "not indexed" routine, not newsworthy.
func (w *pathDebouncer) handleRemove(path string) {
	// The kernel already dropped the watch on the removed dir;
	// Remove here just frees fsnotify's bookkeeping (error means
	// it was never watched — ignore it).
	_ = w.notify.Remove(path)
	rows, err := db.ListFiles(w.database, path)
	if err != nil {
		w.logf("warning: could not list %s: %v", path, err)
		return
	}
	for _, rec := range rows {
		deleted, err := db.DeleteFile(w.database, rec.Path)
		if err != nil {
			w.logf("warning: could not remove %s: %v", rec.Path, err)
			continue
		}
		if deleted {
			w.logf("removed: %s", rec.Path)
		}
	}
}
