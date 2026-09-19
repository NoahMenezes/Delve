package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/scanner"
	"github.com/NoahMenezes/Delve/internal/watcher"
)

var (
	watchDaemon         bool
	watchExtractContent bool
	watchEmbed          bool
)

var watchCmd = &cobra.Command{
	Use:   "watch [path]",
	Short: "Keep the index live as files change",
	Long: `Watch a directory tree and incrementally re-index changed
files: create/write re-runs the scan pipeline for just that file,
remove deletes its row, rename is remove + create. Runs in the
foreground with a live log; stop with Ctrl+C.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// --daemon is a documented stub in this phase. True
		// background daemonization is platform-specific (Linux
		// systemd units, macOS launchd plists, Windows services)
		// and deserves its own phase with per-OS install,
		// logging, and upgrade handling — not a nohup one-liner
		// that orphans the DB lock. Until then: run foreground
		// under your supervisor of choice (systemd, launchd,
		// tmux, nohup) and stop with Ctrl+C / SIGTERM.
		if watchDaemon {
			return errors.New("not implemented: --daemon is a stub (run foreground under systemd/launchd/tmux/nohup for now)")
		}
		if watchEmbed && !watchExtractContent {
			fmt.Fprintln(os.Stderr, "warning: --embed has no effect with --extract-content=false (nothing to embed)")
		}
		root := "."
		if len(args) == 1 {
			root = args[0]
		}

		database, err := db.InitDB()
		if err != nil {
			return err
		}
		defer database.Close()

		// signal.NotifyContext cancels on Ctrl+C (SIGINT) and
		// SIGTERM so the watcher closes fsnotify handles and the
		// deferred database.Close() runs — no corrupted DB from
		// a mid-write kill. SIGKILL still can't be caught (by
		// design); SQLite's WAL-free rollback journal recovers
		// from that on next open.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		return watcher.Watch(ctx, root, database, scanner.ScanOptions{
			ExtractContent: watchExtractContent,
			Embed:          watchEmbed,
		}, func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		})
	},
}

func init() {
	rootCmd.AddCommand(watchCmd)

	watchCmd.Flags().BoolVar(&watchDaemon, "daemon", false, "run in background (stub: not implemented, foreground only)")
	watchCmd.Flags().BoolVar(&watchExtractContent, "extract-content", true, "extract text from txt/md/pdf/docx files (slower, enables content search)")
	watchCmd.Flags().BoolVar(&watchEmbed, "embed", true, "generate meaning vectors for extracted text (slower, enables semantic search)")
}
