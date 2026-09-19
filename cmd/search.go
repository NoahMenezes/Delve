package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
)

// Flags for the search command. Package-level vars bound by pointer in
// init() — the standard Cobra pattern (same as scan's --verbose).
var (
	searchLimit     int
	searchExtension string
)

// searchCmd represents the search command
var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Keyword-search indexed file names, paths, and contents",
	Long: `Full-text search across indexed file names, paths, and
extracted document content (txt/md/pdf/docx) using SQLite FTS5.

Examples:
  delve search invoice
  delve search "quarterly report" --limit 5
  delve search invoice --extension pdf`,
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
}
