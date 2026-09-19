package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/scanner"
)

// verbose controls per-file output. Defined at package level so the
// flag binder (a pointer) stays alive after init() returns — the
// standard Cobra pattern for flags.
var verbose bool

// scanCmd represents the scan command
var scanCmd = &cobra.Command{
	Use:   "scan [path]",
	Short: "Index file metadata from a directory into the local database",
	Long: `Walk a directory recursively and store file metadata
(path, name, extension, size, timestamps) in the local SQLite
index at ~/.delve/delve.db.

No file contents are read in this phase — metadata only.`,
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
		count, err := scanner.ScanDirectory(root, verbose)
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
}
