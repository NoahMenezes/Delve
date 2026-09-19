// Package backend exposes Delve's engine to the Wails frontend.
//
// WAILS BINDING MECHANISM, briefly: the App value constructed here is
// handed to wails.Run in main.go. Every exported method becomes an
// async JavaScript promise at window.go.main.App.<Method>, with
// arguments and return values JSON-marshalled across the bridge.
// Returning a Go error rejects the promise with its message.
//
// Every method below is a thin wrapper: it opens the index, calls one
// existing internal/ function, maps the result into a JSON-friendly
// DTO, and closes the database. No ranking, extraction, or clustering
// logic lives here — the GUI can never drift from the CLI.
package backend

import (
	"strings"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/organize"
	"github.com/NoahMenezes/Delve/internal/scanner"
	"github.com/NoahMenezes/Delve/internal/search"
)

// How many characters of indexed text ride along with each search hit.
// Full content (up to 50k chars × 20 rows) would bloat every keystroke
// response; the detail panel only shows a preview anyway.
const snippetRunes = 500

// App is the bound struct. Zero fields: all state lives in the SQLite
// index, opened per call, so concurrent frontend calls stay safe.
type App struct{}

// New constructs the bound App for main.go.
func New() *App { return &App{} }

// ScanResult reports one finished scan: files indexed and where.
type ScanResult struct {
	Indexed  int    `json:"indexed"`
	Database string `json:"database"`
}

// ScanDirectory indexes root with the same pipeline as `delve scan`.
// Long scans block the promise — the frontend shows an indeterminate
// state until it resolves (per-path progress events are a follow-up).
func (a *App) ScanDirectory(root string, extractContent, embed bool) (ScanResult, error) {
	if root == "" {
		root = "."
	}
	count, err := scanner.ScanDirectory(root, scanner.ScanOptions{
		ExtractContent: extractContent,
		Embed:          embed,
	})
	if err != nil {
		return ScanResult{}, err
	}
	return ScanResult{Indexed: count, Database: db.DefaultDBPath()}, nil
}

// FileDTO is one search hit for the results list. Snippet is a
// preview of the indexed text (see snippetOf); Score is non-nil for
// semantic hits only, mirroring the TUI's SCORE column.
type FileDTO struct {
	Path       string   `json:"path"`
	Name       string   `json:"name"`
	Extension  string   `json:"extension"`
	SizeBytes  int64    `json:"sizeBytes"`
	ModifiedAt int64    `json:"modifiedAt"`
	Snippet    string   `json:"snippet"`
	Score      *float64 `json:"score"`
}

// SearchFiles runs keyword search (FTS5) over names, paths, and
// extracted content — `delve search`, over the bridge.
func (a *App) SearchFiles(query string, limit int) ([]FileDTO, error) {
	database, err := db.InitDB()
	if err != nil {
		return nil, err
	}
	defer database.Close()

	rows, err := db.SearchFiles(database, query, limit, "")
	if err != nil {
		return nil, err
	}
	out := make([]FileDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, fileDTO(r, nil))
	}
	return out, nil
}

// SemanticSearch runs meaning search over local embeddings —
// `delve search --semantic`, over the bridge.
func (a *App) SemanticSearch(query string, limit int) ([]FileDTO, error) {
	database, err := db.InitDB()
	if err != nil {
		return nil, err
	}
	defer database.Close()

	hits, err := search.SemanticSearch(database, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]FileDTO, 0, len(hits))
	for _, h := range hits {
		score := h.Score
		out = append(out, fileDTO(h.Record, &score))
	}
	return out, nil
}

// SuggestionDTO is one dry-run move proposal. There is deliberately
// no "apply" counterpart: the engine cannot move files, so Approved
// below is UI-only selection state echoed back, never acted on.
type SuggestionDTO struct {
	CurrentPath   string  `json:"currentPath"`
	SuggestedPath string  `json:"suggestedPath"`
	Kind          string  `json:"kind"`
	Reason        string  `json:"reason"`
	Score         float64 `json:"score"`
}

// GroupDTO bundles suggestions by destination folder, the way
// `delve organize` prints them.
type GroupDTO struct {
	Destination string          `json:"destination"`
	Suggestions []SuggestionDTO `json:"suggestions"`
}

// OrganizeReportDTO is the full dry-run report plus unmoved
// singletons. ApplyTarget is always empty in this phase — the Apply
// button stays disabled until a future phase teaches the engine to
// move files with explicit confirmation.
type OrganizeReportDTO struct {
	Groups      []GroupDTO      `json:"groups"`
	Unmoved     []SuggestionDTO `json:"unmoved"`
	Total       int             `json:"total"`
	FolderCount int             `json:"folderCount"`
}

// SuggestOrganization previews where indexed files could move —
// `delve organize --dry-run`, over the bridge. Nothing moves.
func (a *App) SuggestOrganization(root string, threshold float64, staleDays int) (OrganizeReportDTO, error) {
	if root == "" {
		root = "."
	}
	database, err := db.InitDB()
	if err != nil {
		return OrganizeReportDTO{}, err
	}
	defer database.Close()

	report, err := organize.SuggestOrganization(database, root, threshold, staleDays)
	if err != nil {
		return OrganizeReportDTO{}, err
	}
	dto := OrganizeReportDTO{Unmoved: []SuggestionDTO{}}
	for dest, sugs := range report.Groups {
		g := GroupDTO{Destination: dest, Suggestions: []SuggestionDTO{}}
		for _, s := range sugs {
			g.Suggestions = append(g.Suggestions, SuggestionDTO{
				CurrentPath:   s.CurrentPath,
				SuggestedPath: s.SuggestedPath,
				Kind:          string(s.Kind),
				Reason:        s.Reason,
				Score:         s.Score,
			})
		}
		dto.Groups = append(dto.Groups, g)
		dto.Total += len(sugs)
	}
	dto.FolderCount = len(report.Groups)
	for _, u := range report.Unmoved {
		dto.Unmoved = append(dto.Unmoved, SuggestionDTO{
			CurrentPath: u.CurrentPath,
			Kind:        string(u.Kind),
			Reason:      u.Reason,
		})
	}
	return dto, nil
}

// fileDTO maps an engine record to its wire form. The snippet is cut
// server-side so the frontend never holds full document text.
func fileDTO(r db.FileRecord, score *float64) FileDTO {
	return FileDTO{
		Path:       r.Path,
		Name:       r.Name,
		Extension:  r.Extension,
		SizeBytes:  r.SizeBytes,
		ModifiedAt: r.ModifiedAt,
		Snippet:    snippetOf(r.Content),
		Score:      score,
	}
}

// snippetOf collapses indexed text to one preview line. Nil content
// (unsupported type, never extracted, failed extraction) says so
// honestly — same rule as the TUI panel.
func snippetOf(content *string) string {
	if content == nil {
		return "(no indexed text — unsupported type or extraction failed)"
	}
	text := strings.Join(strings.Fields(*content), " ")
	runes := []rune(text) // rune slice: never split a multi-byte char
	if len(runes) > snippetRunes {
		return string(runes[:snippetRunes]) + "…"
	}
	if text == "" {
		return "(empty content)"
	}
	return text
}
