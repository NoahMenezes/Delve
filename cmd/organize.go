package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/organize"
)

var (
	organizeDryRun    bool
	organizeThreshold float64
	organizeStaleDays int
)

var organizeCmd = &cobra.Command{
	Use:   "organize [path]",
	Short: "Preview organization suggestions without moving files",
	Long: `Suggest where files could move (type folders, topic clusters,
Archive for stale files) as a dry-run preview. Nothing is moved.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !organizeDryRun {
			return errors.New("file moving is not implemented in this phase — dry-run suggestions only")
		}
		root := "."
		if len(args) == 1 {
			root = args[0]
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		root = abs
		database, err := db.InitDB()
		if err != nil {
			return err
		}
		defer database.Close()
		report, err := organize.SuggestOrganization(database, root, organizeThreshold, organizeStaleDays)
		if err != nil {
			return err
		}
		if len(report.Groups) == 0 {
			fmt.Printf("Everything under %s already looks organized — no suggestions.\n", root)
			printUnmoved(report.Unmoved)
			return nil
		}
		groups := make([]string, 0, len(report.Groups))
		for g := range report.Groups {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		total := 0
		for _, g := range groups {
			sugs := report.Groups[g]
			sort.Slice(sugs, func(i, j int) bool { return sugs[i].CurrentPath < sugs[j].CurrentPath })
			fmt.Printf("== %s/\n", g)
			for _, s := range sugs {
				fmt.Printf("  %s -> %s  (%s)\n", s.CurrentPath, s.SuggestedPath, s.Reason)
			}
			total += len(sugs)
		}
		printUnmoved(report.Unmoved)
		fmt.Printf("%d files suggested to move across %d new folders\n", total, len(report.Groups))
		return nil
	},
}

func printUnmoved(unmoved []organize.OrganizeSuggestion) {
	if len(unmoved) == 0 {
		return
	}
	sort.Slice(unmoved, func(i, j int) bool { return unmoved[i].CurrentPath < unmoved[j].CurrentPath })
	fmt.Println("-- already unique:")
	for _, u := range unmoved {
		fmt.Printf("  -- already unique: %s  (%s)\n", u.CurrentPath, u.Reason)
	}
}

func init() {
	rootCmd.AddCommand(organizeCmd)
	organizeCmd.Flags().BoolVar(&organizeDryRun, "dry-run", true, "preview suggestions without moving files (only mode supported)")
	organizeCmd.Flags().Float64Var(&organizeThreshold, "threshold", 0.75, "cosine similarity threshold for topic grouping")
	organizeCmd.Flags().IntVar(&organizeStaleDays, "stale-days", 180, "days since modification after which a file counts as stale")
}
