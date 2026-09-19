/*
Copyright © 2026 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/tui"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "delve",
	Short: "Local-first, AI-powered file browser CLI",
	Long: `Delve indexes files locally and lets users search by meaning,
entirely offline — no account, no subscription, no cloud calls ever.`,
	// Bare `delve` opens the interactive browser (Phase 7). Flags
	// like --help never reach here: Cobra serves them first, so
	// `delve --help` keeps printing help as normal.
	RunE: func(cmd *cobra.Command, args []string) error {
		return tui.Run()
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// No global flags yet. Command-specific flags (like scan's
	// --verbose) are defined on their own commands.
}
