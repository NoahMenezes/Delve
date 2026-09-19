package backend

import (
	"os"
	"path/filepath"
	"testing"
)

// isolatedHome points HOME at a temp dir so tests never touch the
// user's real ~/.delve index. InitDB resolves the database (and the
// model cache) from home, so this one redirect sandboxes everything.
func isolatedHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanThenSearch drives the two core bindings against the real
// engine: ScanDirectory indexes a fixture, SearchFiles finds it back
// with a server-side snippet. Metadata-only scan keeps it fast and
// offline (no model download).
func TestScanThenSearch(t *testing.T) {
	isolatedHome(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "invoice.txt"), "quarterly invoice for taxes")

	app := New()
	res, err := app.ScanDirectory(root, false, false)
	if err != nil {
		t.Fatalf("ScanDirectory: %v", err)
	}
	if res.Indexed != 1 {
		t.Fatalf("indexed = %d, want 1", res.Indexed)
	}

	hits, err := app.SearchFiles("invoice", 20)
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Name != "invoice.txt" {
		t.Fatalf("name = %q, want invoice.txt", hits[0].Name)
	}
	if hits[0].Score != nil {
		t.Fatalf("keyword hit carries a score — must be nil")
	}
	// Metadata-only scan: no content extracted, snippet must say so
	// honestly instead of rendering blank.
	if hits[0].Snippet == "" {
		t.Fatalf("empty snippet with no honesty message")
	}
}

// TestSuggestOrganization checks the dry-run binding: a stale zip and
// a fresh txt must come back grouped with reasons, and nothing may
// move on disk.
func TestSuggestOrganization(t *testing.T) {
	isolatedHome(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), "hello")
	old := filepath.Join(root, "old.zip")
	writeFile(t, old, "data")

	app := New()
	if _, err := app.ScanDirectory(root, false, false); err != nil {
		t.Fatalf("ScanDirectory: %v", err)
	}

	report, err := app.SuggestOrganization(root, 0.75, 180)
	if err != nil {
		t.Fatalf("SuggestOrganization: %v", err)
	}
	if report.Total == 0 || report.FolderCount == 0 {
		t.Fatalf("empty report: %+v", report)
	}
	for _, g := range report.Groups {
		for _, s := range g.Suggestions {
			if s.Reason == "" {
				t.Fatalf("suggestion without reason: %+v", s)
			}
		}
	}
	// Dry-run proof: the fixture still has exactly its 2 files.
	var count int
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			count++
		}
		return nil
	})
	if count != 2 {
		t.Fatalf("files moved during dry-run: %d on disk", count)
	}
}

// TestSnippetOf is the pure unit half: nil/empty/long content shapes.
func TestSnippetOf(t *testing.T) {
	if s := snippetOf(nil); s == "" {
		t.Fatalf("nil content renders blank")
	}
	text := "hello"
	if s := snippetOf(&text); s != "hello" {
		t.Fatalf("short text mangled: %q", s)
	}
}
