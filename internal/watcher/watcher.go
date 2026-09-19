// Package watcher keeps Delve's index live: it watches a directory
// tree and incrementally re-indexes changed files instead of
// requiring manual re-scans.
package watcher

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/NoahMenezes/Delve/internal/scanner"
)

// Watch follows root recursively until ctx is cancelled, keeping the
// index up to date file-by-file. All indexing flows through the same
// scanner.ScanFile / db.DeleteFile pipeline as `delve scan`, so watch
// and scan can never drift. logf receives live log lines
// ("indexed: x", "removed: y", "watch error: ..."); pass nil to mute.
//
// FSNOTIFY RECURSION, explained (the most common fsnotify mistake):
// fsnotify watches exactly the paths you Add — it never follows
// subdirectories on its own. So Watch walks the tree on startup and
// Adds every subdirectory, and when a new directory appears mid-watch
// it Adds that too (see pathDebouncer.processDir in debounce.go).
// Files need no Add: watching their parent directory delivers their
// Create/Write/Remove/Rename events. Skipped directories
// (scanner.ShouldSkipDir: .git, node_modules, ...) are never Added,
// so their files stay invisible, exactly like during a scan.
func Watch(ctx context.Context, root string, database *sql.DB, opts scanner.ScanOptions, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absRoot); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("not a directory: %s (delve watch takes a directory, not a file)", absRoot)
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer notify.Close()

	// Startup: one Add per subdirectory. A single Add on the root
	// would miss every event below it — this walk is the recursion.
	if err := addRecursive(notify, absRoot); err != nil {
		return err
	}
	logf("watching %s (Ctrl+C to stop)", absRoot)

	w := &pathDebouncer{
		notify:   notify,
		database: database,
		opts:     opts,
		logf:     logf,
		timers:   map[string]*time.Timer{},
	}
	defer w.stopAll()

	for {
		select {
		case <-ctx.Done():
			// Pending debounced paths (<500ms old) are dropped:
			// the next scan or watch run picks them up. Failing
			// to flush is safe because every event is idempotent.
			return nil
		case err := <-notify.Errors:
			logf("watch error: %v", err)
		case ev := <-notify.Events:
			// Chmod changes no content worth re-embedding; ignore
			// it so permission-only touches stay silent.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			w.schedule(ev.Name)
		}
	}
}

// addRecursive Adds dir and every non-skipped subdirectory. The root
// itself is always watched even if its base name is on the skip list
// (the user asked for it explicitly); only children are pruned.
func addRecursive(notify *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corner: skip, like the scanner
		}
		if !entry.IsDir() {
			return nil
		}
		if p != dir && scanner.ShouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}
		// Adding twice is harmless (fsnotify dedupes), so the
		// startup walk and processDir can overlap without coordination.
		if err := notify.Add(p); err != nil {
			return err
		}
		return nil
	})
}
