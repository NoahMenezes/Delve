// View rendering for the interactive browser: search bar, results
// list, read-only detail panel, and the status/help bar.
package tui

import (
	"fmt"
	"strings"
	"time"
)

// View assembles the screen top to bottom: query bar with the mode
// indicator, results (or an empty/error/status line), the detail
// viewport when expanded, and a one-line key cheat sheet.
func (m Model) View() string {
	var out strings.Builder

	mode := "[keyword]"
	if m.mode == modeSemantic {
		mode = "[semantic]"
	}
	out.WriteString(fmt.Sprintf("%s delve %s\n", m.input.View(), mode))

	switch {
	case m.errMsg != "":
		out.WriteString("  error: " + m.errMsg + "\n")
	case m.input.Value() == "" && len(m.results) == 0:
		out.WriteString("  type to search — tab switches keyword/semantic\n")
	case m.searching:
		out.WriteString(fmt.Sprintf("  %s searching…\n", m.spinner.View()))
	case len(m.results) == 0:
		if m.mode == modeSemantic {
			out.WriteString("  no semantic results (need embedded files? run `delve scan <dir>`)\n")
		} else {
			out.WriteString("  no results\n")
		}
	default:
		for i, r := range m.results {
			cursor := "  "
			if i == m.cursor {
				cursor = "▸ "
			}
			if r.hasScore {
				fmt.Fprintf(&out, "%s%.3f  %s  %s\n", cursor, r.score, r.record.Name, r.record.Path)
			} else {
				fmt.Fprintf(&out, "%s%s  %s\n", cursor, r.record.Name, r.record.Path)
			}
		}
	}

	if m.expanded && len(m.results) > 0 {
		out.WriteString("\n" + m.viewport.View() + "\n")
	}

	out.WriteString(fmt.Sprintf("\n%d results · ↑/↓ move · enter details · tab mode · esc clear · ctrl+c quit",
		len(m.results)))
	return out.String()
}

// detailBody renders the read-only panel for one row: full path,
// size, modified date, and a content preview from the indexed text.
// The preview comes from the database (possibly stale vs the live
// file) — it describes what the index knows, which is what search
// ranked.
func detailBody(r result) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n", r.record.Path)
	fmt.Fprintf(&out, "size %s · modified %s · type %s\n",
		humanSize(r.record.SizeBytes), relativeTime(r.record.ModifiedAt), extOrNone(r.record.Extension))
	if r.hasScore {
		fmt.Fprintf(&out, "semantic score %.3f\n", r.score)
	}
	out.WriteString("\n" + previewSnippet(r) + "\n")
	return out.String()
}

// previewSnippet returns the first snippetRunes of the indexed
// content with whitespace collapsed. Nil content (unsupported type,
// never extracted, extraction failed) says so honestly instead of
// showing a blank panel.
func previewSnippet(r result) string {
	if r.record.Content == nil {
		return "(no indexed text — unsupported type or extraction failed)"
	}
	text := strings.Join(strings.Fields(*r.record.Content), " ")
	runes := []rune(text) // rune slice: never split a multi-byte char
	if len(runes) > snippetRunes {
		return string(runes[:snippetRunes]) + "…"
	}
	if text == "" {
		return "(empty content)"
	}
	return text
}

func extOrNone(ext string) string {
	if ext == "" {
		return "(none)"
	}
	return ext
}

// humanSize and relativeTime are display twins of cmd/search.go's
// unexported helpers. Duplicated (not imported) because internal/tui
// cannot import cmd — cmd will import internal/tui for bare `delve`,
// and the reverse edge would be an import cycle. Keep the two copies
// in sync; formatting changes belong in both.
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value)
}

func relativeTime(unix int64) string {
	then := time.Unix(unix, 0)
	age := time.Since(then)
	switch {
	case age < 0:
		return then.Format("2006-01-02")
	case age < time.Hour:
		mins := int(age.Minutes())
		if mins <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case age < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(age.Hours()))
	case age < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(age.Hours()/24))
	default:
		return then.Format("2006-01-02")
	}
}
