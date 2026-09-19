// Package scanner walks a directory tree and upserts file metadata
// into Delve's SQLite index. Phase 1 scope: metadata only —
// no file contents are opened or read.
package scanner

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoahMenezes/Delve/internal/db"
)

// skipDirs is the small, easily-extensible blocklist of directory
// names we never descend into. These are conventionally-ignored,
// high-noise, low-value trees (VCS data, dependency caches, build
// output) that would bloat the index and slow scans to a crawl.
// Match is on the base name only, so it applies at any depth.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".svn":         true,
	".hg":          true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"target":       true, // Rust / Maven build output
	"dist":         true,
	"build":        true,
	".delve":       true, // never index our own database directory
}

// ScanDirectory walks root recursively and upserts every regular file
// into the SQLite index. It returns how many files were indexed.
//
// The verbose flag controls per-file output: false prints just a
// running count (one line, overwritten in place), true prints each
// indexed path on its own line (useful for debugging, noisy at scale).
//
// NOTE on the signature: the Phase 1 brief sketched
// ScanDirectory(root string), but verbose lives here (rather than in
// cmd/) so the progress-printing policy stays with the walk logic
// and stays testable in one place.
func ScanDirectory(root string, verbose bool) (fileCount int, err error) {
	// Resolve early: filepath.WalkDir would otherwise report a
	// confusing error deep inside the walk for a bad root.
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("path does not exist: %s", root)
		}
		return 0, fmt.Errorf("cannot stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("not a directory: %s (delve scan takes a directory, not a file)", root)
	}

	// Abs keeps stored paths stable no matter where the user runs
	// delve from ("delve scan ." vs "delve scan /home/u/docs").
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, fmt.Errorf("cannot resolve absolute path for %s: %w", root, err)
	}

	database, err := db.InitDB()
	if err != nil {
		return 0, fmt.Errorf("cannot open database: %w", err)
	}
	defer database.Close()

	// filepath.WalkDir is preferred over the older filepath.Walk:
	// it takes an fs.DirEntry (not an os.FileInfo), so for most
	// entries it avoids an extra lstat syscall — measurably faster
	// on large trees. Returning fs.SkipDir from the callback prunes
	// a whole subtree; returning any other error aborts the walk,
	// so for per-file problems we log + return nil to keep going.
	walkFn := func(path string, entry fs.DirEntry, walkErr error) error {
		// walkErr != nil means Go couldn't even list this path
		// (most often: permission denied on a subdirectory).
		// Warn and continue — one unreadable corner must never
		// abort a whole scan.
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, walkErr)
			return nil
		}

		// Prune noise directories before descending. entry.IsDir
		// is cheap (no stat call); SkipDir tells WalkDir to skip
		// the entire subtree.
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				if verbose {
					fmt.Printf("skipping directory: %s\n", path)
				}
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks for now. A symlink's DirEntry reports its
		// own type, and following links risks cycles (A -> B -> A)
		// and double-indexing one file under many paths. Explicit
		// symlink-following (with cycle detection) is a later-phase
		// feature, not Phase 1 scope.
		// KNOWN LIMITATION: symlinked files AND symlinked dirs are
		// both skipped, so files reachable only via symlink are
		// invisible to search until that feature lands.
		if entry.Type()&fs.ModeSymlink != 0 {
			if verbose {
				fmt.Printf("skipping symlink: %s\n", path)
			}
			return nil
		}

		// entry.Info() does stat the file (one syscall) — this is
		// where permission errors on individual files surface.
		fileInfo, err := entry.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
			return nil
		}
		// Belt-and-suspenders: non-regular files (devices, sockets,
		// pipes) have no meaningful size/mtime for our index.
		if !fileInfo.Mode().IsRegular() {
			return nil
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
			return nil
		}

		rec := toFileRecord(absPath, fileInfo)
		if err := db.UpsertFile(database, rec); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not index %s: %v\n", path, err)
			return nil
		}

		fileCount++
		if verbose {
			fmt.Printf("indexed: %s\n", absPath)
		} else {
			// \r rewrites the same terminal line so a 100k-file
			// scan shows a live counter instead of 100k lines.
			// (Redirected to a file it just looks like many lines —
			// harmless, and --verbose exists for exact logging.)
			fmt.Printf("\rIndexed %d files...", fileCount)
		}
		return nil
	}

	if err := filepath.WalkDir(absRoot, walkFn); err != nil {
		// Only reached for fatal walk errors (e.g. root became
		// unreadable mid-scan); per-file issues already continued above.
		return fileCount, fmt.Errorf("scan aborted: %w", err)
	}

	if !verbose {
		fmt.Println() // end the \r counter line cleanly
	}
	return fileCount, nil
}

// toFileRecord converts a path + FileInfo into a db.FileRecord.
//
// CROSS-PLATFORM CREATION-TIME CAVEAT (the trickiest part of Phase 1):
// Go's stdlib fs.FileInfo exposes only ModTime(). There is no portable
// "creation time" accessor because the OS support differs wildly:
//   - Windows (NTFS): real birth time, exposed via syscalls.
//   - macOS (APFS/HFS+): birth time available as Birthtimespec.
//   - Linux (ext4): birth time (btime/statx STATX_BTIME) exists only on
//     newer kernels + filesystems, and Go doesn't surface it.
//
// A "correct on every OS" implementation would need per-OS syscall
// code behind build tags — overkill for a metadata foundation. So we
// fall back to ModTime for CreatedAt and document it. Later phases
// that need true birth time can add platform-specific files
// (e.g. created_windows.go / created_darwin.go) without changing the schema.
func toFileRecord(absPath string, info fs.FileInfo) db.FileRecord {
	modTime := info.ModTime()
	createdAt := modTime // fallback — see caveat above
	now := time.Now().Unix()

	return db.FileRecord{
		Path:        absPath,
		Name:        info.Name(),
		Extension:   strings.ToLower(filepath.Ext(info.Name())),
		SizeBytes:   info.Size(),
		ModifiedAt:  modTime.Unix(),
		CreatedAt:   createdAt.Unix(),
		IndexedAt:   now,
		ContentHash: nil, // Phase 1: metadata only, hashing comes later
	}
}
