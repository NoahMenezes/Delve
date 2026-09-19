// Package organize is Delve's dry-run suggestion engine: it proposes
// where files *could* move (type folders, topic clusters, an Archive
// for stale files) without moving anything. ZERO file-moving
// capability by design — no os file-writing APIs are imported,
// destinations are built with filepath.Join only, and callers decide
// what to do with the report.
package organize

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/search"
)

// OrganizeKind names which rule produced a suggestion. Priority on
// conflict is stale > topic > type-group: a file gets exactly ONE
// suggestion, and Reason records the winning rule only.
type OrganizeKind string

const (
	KindTypeGroup OrganizeKind = "type-group"
	KindTopic     OrganizeKind = "topic"
	KindStale     OrganizeKind = "stale"
)

// OrganizeSuggestion is one proposed move. Score is the cosine
// similarity for topic matches, 1.0 for deterministic rules (stale,
// type-group). Group is the destination directory (Groups map key).
type OrganizeSuggestion struct {
	CurrentPath   string
	SuggestedPath string
	Kind          OrganizeKind
	Reason        string
	Score         float64
	Group         string
}

// OrganizeReport is the full dry-run result: Groups maps a destination
// directory to its suggestions; Unmoved holds embedded files with
// unique content (singleton clusters) that fit no group.
type OrganizeReport struct {
	Groups  map[string][]OrganizeSuggestion
	Unmoved []OrganizeSuggestion
}

// TypeDirs maps lowercase dotted extensions (FileRecord.Extension
// form) to top-level folders; typeDir falls back to "Other".
var TypeDirs = map[string]string{
	".png": "Images", ".jpg": "Images", ".jpeg": "Images", ".gif": "Images", ".bmp": "Images", ".svg": "Images", ".webp": "Images", ".tiff": "Images", ".heic": "Images", ".ico": "Images",
	".pdf": "Documents", ".doc": "Documents", ".docx": "Documents", ".txt": "Documents", ".md": "Documents", ".rtf": "Documents", ".odt": "Documents", ".xls": "Documents", ".xlsx": "Documents", ".csv": "Documents", ".ppt": "Documents", ".pptx": "Documents", ".tex": "Documents",
	".zip": "Archives", ".tar": "Archives", ".gz": "Archives", ".tgz": "Archives", ".rar": "Archives", ".7z": "Archives", ".bz2": "Archives", ".xz": "Archives",
	".go": "Code", ".py": "Code", ".js": "Code", ".ts": "Code", ".tsx": "Code", ".jsx": "Code", ".java": "Code", ".c": "Code", ".h": "Code", ".cpp": "Code", ".rs": "Code", ".rb": "Code", ".php": "Code", ".swift": "Code", ".kt": "Code", ".cs": "Code", ".html": "Code", ".css": "Code", ".json": "Code", ".xml": "Code", ".yml": "Code", ".yaml": "Code", ".toml": "Code", ".sh": "Code", ".sql": "Code",
	".mp3": "Audio", ".wav": "Audio", ".flac": "Audio", ".ogg": "Audio", ".m4a": "Audio", ".aac": "Audio",
	".mp4": "Video", ".mkv": "Video", ".avi": "Video", ".mov": "Video", ".webm": "Video", ".m4v": "Video",
}

// typeDir maps an extension to its folder, defaulting to "Other".
func typeDir(ext string) string {
	if dir, ok := TypeDirs[strings.ToLower(ext)]; ok {
		return dir
	}
	return "Other"
}

// SuggestOrganization builds a dry-run report for indexed files under
// root (absolute path; rows outside it are ignored). Passes run in
// priority order — stale, then topic, then type-group fallback — and
// the assigned set enforces one suggestion per file.
//
// Coverage: db.ListFiles sees EVERY indexed row, so stale and
// type-group apply to all files; only topic clustering needs vectors
// (unembedded rows fall back to type-group with an honest reason).
func SuggestOrganization(database *sql.DB, root string, threshold float64, staleDays int) (OrganizeReport, error) {
	report := OrganizeReport{Groups: map[string][]OrganizeSuggestion{}}
	abs, err := filepath.Abs(root)
	if err != nil {
		return report, err
	}
	root = filepath.Clean(abs)
	if threshold <= 0 {
		threshold = 0.75 // default topic threshold; see cluster.go
	}
	if staleDays <= 0 {
		staleDays = 180
	}

	// All rows under root (ListFiles already filters by prefix) plus
	// a vector lookup for the embedded subset.
	rows, err := db.ListFiles(database, root)
	if err != nil {
		return report, err
	}
	allVecs, err := db.GetAllEmbeddings(database)
	if err != nil {
		return report, err
	}
	vecByPath := map[string]db.StoredEmbedding{}
	for _, se := range allVecs {
		if p := se.Record.Path; p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			vecByPath[p] = se
		}
	}

	used := map[string]bool{}     // claimed names per destination dir
	assigned := map[string]bool{} // paths already given a suggestion
	add := func(s OrganizeSuggestion) {
		if s.SuggestedPath == s.CurrentPath {
			return // already organized: omit entirely, reserve nothing
		}
		dir := filepath.Dir(s.SuggestedPath)
		s.SuggestedPath = uniqueDest(dir, filepath.Base(s.SuggestedPath), used)
		s.Group = dir
		report.Groups[dir] = append(report.Groups[dir], s)
	}

	// Pass 1 — stale beats every other rule. Future mtimes (clock skew)
	// are never stale: ModifiedAt must also be <= now.
	now := time.Now().Unix()
	cutoff := now - int64(staleDays)*86400
	var fresh []db.FileRecord
	for _, rec := range rows {
		mt := rec.ModifiedAt
		if mt < cutoff && mt <= now {
			add(OrganizeSuggestion{
				CurrentPath:   rec.Path,
				SuggestedPath: filepath.Join(root, "Archive", typeDir(rec.Extension), rec.Name),
				Kind:          KindStale,
				Reason:        fmt.Sprintf("not modified in %d days (stale threshold %d)", (now-mt)/86400, staleDays),
				Score:         1.0,
			})
			assigned[rec.Path] = true
		} else {
			fresh = append(fresh, rec)
		}
	}

	// Pass 2 — topic clustering over the non-stale remainder. Rows
	// without a usable vector (unembedded, or dim mismatch) fall back
	// to type-group with an honest reason instead of being skipped.
	var clusterable []db.StoredEmbedding
	for _, rec := range fresh {
		se, ok := vecByPath[rec.Path]
		if !ok || len(se.Vector) == 0 {
			add(OrganizeSuggestion{
				CurrentPath:   rec.Path,
				SuggestedPath: filepath.Join(root, typeDir(rec.Extension), rec.Name),
				Kind:          KindTypeGroup,
				Reason:        fmt.Sprintf("extension %s -> %s", rec.Extension, typeDir(rec.Extension)),
				Score:         1.0,
			})
			assigned[rec.Path] = true
		} else {
			// Refresh the record from ListFiles (same row, canonical).
			se.Record = rec
			clusterable = append(clusterable, se)
		}
	}
	clusters, err := clusterEmbeddings(clusterable, threshold)
	if err != nil {
		return report, err
	}
	topicNo := 0
	for _, c := range clusters {
		if len(c.members) <= 1 {
			continue // singletons handled in pass 3
		}
		topicNo++ // NN counts multi-member clusters only
		tag := fmt.Sprintf("topic-%02d", topicNo)
		exemplar := c.members[0].Record.Name
		for _, m := range c.members {
			if assigned[m.Record.Path] {
				continue // stale/fallback already claimed it
			}
			score := search.Cosine(m.Vector, c.rep) // vs FINAL rep, after all joins
			dir := filepath.Join(root, typeDir(m.Record.Extension), tag)
			add(OrganizeSuggestion{
				CurrentPath:   m.Record.Path,
				SuggestedPath: filepath.Join(dir, m.Record.Name),
				Kind:          KindTopic,
				Reason:        fmt.Sprintf("topic match (cosine %.2f) with '%s' in %s", score, exemplar, tag),
				Score:         score,
			})
			assigned[m.Record.Path] = true
		}
	}

	// Pass 3 — singleton clusters are unique content: Unmoved, never a
	// type-group guess. Kind stays topic (the topic stage judged it);
	// the empty SuggestedPath marks "no move".
	for _, c := range clusters {
		if len(c.members) != 1 {
			continue
		}
		m := c.members[0]
		if assigned[m.Record.Path] {
			continue
		}
		report.Unmoved = append(report.Unmoved, OrganizeSuggestion{
			CurrentPath: m.Record.Path,
			Kind:        KindTopic,
			Reason:      "unique content, no similar files found",
		})
	}
	return report, nil
}

// uniqueDest resolves collisions within one destination directory: the
// second "main.go" becomes "main-2.go" (suffix before the extension).
// used is report-local — dry-run never stats the filesystem, and files
// already in place are omitted (not reserved), so apply with care.
func uniqueDest(dir, name string, used map[string]bool) string {
	key := dir + "\x00" + name
	if !used[key] {
		used[key] = true
		return filepath.Join(dir, name)
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if key := dir + "\x00" + cand; !used[key] {
			used[key] = true
			return filepath.Join(dir, cand)
		}
	}
}
