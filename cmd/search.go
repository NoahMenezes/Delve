package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/search"
)

// Flags for the search command. Package-level vars bound by pointer in
// init() — the standard Cobra pattern (same as scan's --verbose).
var (
	searchLimit     int
	searchExtension string
	searchSemantic  bool
)

// searchCmd represents the search command
var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search indexed files by keyword or by meaning",
	Long: `Full-text search across indexed file names, paths, and
extracted document content (txt/md/pdf/docx) using SQLite FTS5 —
or, with --semantic, meaning-based search over local embeddings.

Keyword finds exact matches (great for filename fragments);
semantic finds related ideas with no shared words. Both run
fully offline.

Examples:
  delve search invoice
  delve search "quarterly report" --limit 5
  delve search invoice --extension pdf
  delve search --semantic "documents about vacation planning"`,
	// At least one word of query is required; multiple words are
	// space-joined ("delve search quarterly report" just works).
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.Join(args, " ")
		if searchLimit <= 0 {
			searchLimit = 20 // keep the display hint honest; db layer defaults the same way
		}

		database, err := db.InitDB()
		if err != nil {
			return err
		}
		defer database.Close()

		// Semantic search is additive, not a replacement: exact-match
		// keyword search stays the default, --semantic routes to the
		// embedding index for meaning-based ranking.
		if searchSemantic {
			return runSemanticSearch(database, query)
		}

		results, err := db.SearchFiles(database, query, searchLimit, searchExtension)
		if err != nil {
			return fmt.Errorf("search failed: %w", err)
		}

		if len(results) == 0 {
			fmt.Printf("No results for %q.\n", query)
			fmt.Println("Tip: search matches file names, paths, and extracted document text. Try `delve scan <dir>` if the file isn't indexed.")
			return nil
		}

		printResults(results)
		if len(results) == searchLimit {
			fmt.Printf("(showing first %d — narrow with --limit or --extension)\n", searchLimit)
		}
		return nil
	},
}

// runSemanticSearch executes the --semantic path: embed the query,
// rank by cosine similarity, and print hits with their scores.
func runSemanticSearch(database *sql.DB, query string) error {
	hits, err := search.SemanticSearch(database, query, searchLimit)
	if err != nil {
		return fmt.Errorf("semantic search failed: %w", err)
	}
	if len(hits) == 0 {
		fmt.Printf("No semantic results for %q.\n", query)
		fmt.Println("Tip: semantic search needs embedded files — run `delve scan <dir>` (embedding is on by default).")
		return nil
	}
	printSemanticResults(hits)
	if len(hits) == searchLimit {
		fmt.Printf("(showing first %d — narrow with --limit)\n", searchLimit)
	}
	return nil
}

// printSemanticResults renders meaning-ranked hits. Same table idiom
// as keyword results, plus the SCORE column (cosine similarity,
// higher = more similar) that makes semantic ranking interpretable.
func printSemanticResults(hits []search.Hit) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tNAME\tSIZE\tMODIFIED\tPATH")
	for _, h := range hits {
		fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%s\n",
			h.Score,
			h.Record.Name,
			humanSize(h.Record.SizeBytes),
			relativeTime(h.Record.ModifiedAt),
			h.Record.Path,
		)
	}
	w.Flush()
}

// printResults renders matches as an aligned table. tabwriter (stdlib)
// pads columns with tabs — no third-party table library needed.
func printResults(results []db.FileRecord) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSIZE\tMODIFIED\tPATH")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			r.Name,
			humanSize(r.SizeBytes),
			relativeTime(r.ModifiedAt),
			r.Path,
		)
	}
	w.Flush()
}

// humanSize renders raw bytes as "2.3 MB". Sizes stay int64 everywhere
// else; formatting happens only at display time.
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

// relativeTime renders a Unix timestamp as "3 days ago" for recent
// files, falling back to an absolute date (2006-01-02) past ~30 days —
// relative labels stop being useful once you can't feel the distance.
// (The "2006-01-02" layout looks odd: Go formats times with a reference
// date — Mon Jan 2 15:04:05 MST 2006 — instead of strftime codes.)
func relativeTime(unix int64) string {
	then := time.Unix(unix, 0)
	age := time.Since(then)

	switch {
	case age < 0:
		// Clock skew / future mtime: don't print "in -3 hours".
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

func init() {
	rootCmd.AddCommand(searchCmd)

	searchCmd.Flags().IntVar(&searchLimit, "limit", 20, "maximum number of results to show")
	searchCmd.Flags().StringVar(&searchExtension, "extension", "", "filter by file extension (e.g. --extension pdf)")
	searchCmd.Flags().BoolVar(&searchSemantic, "semantic", false, "search by meaning using local embeddings instead of keywords")
}
