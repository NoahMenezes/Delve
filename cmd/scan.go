package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/scanner"
)

// Scan flags. Package-level vars bound by pointer in init() — the
// standard Cobra pattern (binders must outlive init's return).
var verbose bool
var extractContent bool
var embedContent bool

// scanCmd represents the scan command
var scanCmd = &cobra.Command{
	Use:   "scan [path]",
	Short: "Index files from a directory into the local database",
	Long: `Walk a directory recursively and store file metadata
(path, name, extension, size, timestamps), extracted document text
(txt/md/pdf/docx), and meaning vectors for semantic search in the
local SQLite index at ~/.delve/delve.db.

First run with extraction+embedding downloads the model and runtime
once (~50MB); everything after is offline.

Use --extract-content=false for a faster metadata-only scan, and
--embed=false to skip meaning vectors (keyword search still works).`,
	// MaximumNArgs(1) gives a clean Cobra error for
	// `delve scan a b` instead of silently ignoring extras.
	Args: cobra.MaximumNArgs(1),
	// RunE (not Run) so we can return errors and let Cobra print
	// them with the usage line — friendlier than fmt.Println+exit.
	RunE: func(cmd *cobra.Command, args []string) error {
		// Default to the current directory when no path is given,
		// so bare `delve scan` just works.
		root := "."
		if len(args) == 1 {
			root = args[0]
		}

		start := time.Now()
		count, err := scanner.ScanDirectory(root, scanner.ScanOptions{
			Verbose:        verbose,
			ExtractContent: extractContent,
			Embed:          embedContent,
		})
		if err != nil {
			return err
		}
		elapsed := time.Since(start).Round(time.Millisecond)

		fmt.Printf("Indexed %d files in %s\n", count, elapsed)
		fmt.Printf("Database: %s\n", db.DefaultDBPath())
		return nil
	},
}

func init() {
	rootCmd.AddCommand(scanCmd)

	// --verbose prints every indexed path; default prints just a
	// running counter so large scans don't flood the terminal.
	scanCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "print each indexed file path")
	// Extraction is I/O-heavy; metadata-only scans stay fast.
	scanCmd.Flags().BoolVar(&extractContent, "extract-content", true, "extract text from txt/md/pdf/docx files (slower, enables content search)")
	// Embedding runs local neural inference per file — the slowest
	// step, but the one that powers --semantic search.
	scanCmd.Flags().BoolVar(&embedContent, "embed", true, "generate meaning vectors for extracted text (slower, enables semantic search)")
}
