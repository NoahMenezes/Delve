// Package scanner walks a directory tree and upserts file metadata
// into Delve's SQLite index, optionally extracting document text
// (Phase 3) for supported formats.
package scanner

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/embed"
	"github.com/NoahMenezes/Delve/internal/extract"
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

// ScanOptions bundles the per-phase scan flags. The signature grew
// one boolean per phase (verbose, extractContent, now embed) until
// positional bools became unreadable — this struct is the graduation
// the Phase 3 comment promised. New phases add fields, not params.
type ScanOptions struct {
	Verbose        bool // print each indexed path instead of a counter
	ExtractContent bool // Phase 3: extract txt/md/pdf/docx text
	Embed          bool // Phase 4: embed extracted text for semantic search
}

// ScanDirectory walks root recursively and upserts every regular file
// into the SQLite index. It returns how many files were indexed.
func ScanDirectory(root string, opts ScanOptions) (fileCount int, err error) {
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
				if opts.Verbose {
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
			if opts.Verbose {
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

		// Phase 3: extract document text for supported formats. This
		// runs AFTER the metadata upsert on purpose — extraction is
		// the fallible part (corrupt PDFs, unreadable zips), and a
		// file with broken content must still be indexed by name.
		// Failures record (nil content + timestamp) so the database
		// distinguishes "tried and failed" from "not yet tried" and
		// doesn't waste work retrying known-bad files every scan.
		if opts.ExtractContent && extract.IsSupported(rec.Extension) {
			extractFile(database, absPath, path, rec.Extension, opts)
		}

		fileCount++
		if opts.Verbose {
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

	if !opts.Verbose {
		fmt.Println() // end the \r counter line cleanly
	}
	return fileCount, nil
}

// extractFile extracts one file's text, stores it unless unchanged,
// and embeds it when appropriate. Change detection comes first: if
// the new text hashes equal to the stored hash, the UPDATE is skipped
// entirely (no pointless FTS re-index), and embedding runs only when
// no vector exists yet (e.g. a previous --embed=false scan).
func extractFile(database *sql.DB, absPath, displayPath, extension string, opts ScanOptions) {
	text, err := extract.ExtractText(absPath, extension)
	now := time.Now().Unix()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not extract content from %s: %v\n", displayPath, err)
		if uerr := db.UpdateFileContent(database, absPath, nil, nil, now); uerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record extraction failure for %s: %v\n", displayPath, uerr)
		}
		return
	}

	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	if stored := db.GetFileContentHash(database, absPath); stored != "" && stored == hash {
		if opts.Verbose {
			fmt.Printf("content unchanged, skipping: %s\n", absPath)
		}
		// Content identical — but a vector may still be missing.
		if opts.Embed && strings.TrimSpace(text) != "" {
			if fresh, err := db.EmbeddingStatus(database, absPath, 0); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not check embedding status for %s: %v\n", absPath, err)
			} else if !fresh {
				embedFile(database, absPath, text, opts.Verbose)
			}
		}
		return
	}

	if uerr := db.UpdateFileContent(database, absPath, &text, &hash, now); uerr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not store content for %s: %v\n", displayPath, uerr)
		return
	}
	if opts.Verbose {
		fmt.Printf("extracted content: %s (%d chars)\n", absPath, len([]rune(text)))
	}
	// Empty text (e.g. scanned PDFs) has nothing to embed — skip it,
	// but its extracted_at stamp stands so we don't retry every scan.
	if opts.Embed && strings.TrimSpace(text) != "" {
		embedFile(database, absPath, text, opts.Verbose)
	}
}

// embedFile generates and stores a file's meaning vector. Embed
// failures warn and continue: a file without a vector is simply
// invisible to semantic search, still fully searchable by keyword.
func embedFile(database *sql.DB, absPath, text string, verbose bool) {
	vec, err := embed.GenerateEmbedding(text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not embed %s: %v\n", absPath, err)
		return
	}
	if err := db.UpsertEmbedding(database, absPath, vec, time.Now().Unix()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not store embedding for %s: %v\n", absPath, err)
	} else if verbose {
		fmt.Printf("embedded: %s (%d dims)\n", absPath, len(vec))
	}
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
		ContentHash: nil, // INSERT-time only; UpdateFileContent owns the hash afterwards (see db.UpsertFile note)
	}
}
