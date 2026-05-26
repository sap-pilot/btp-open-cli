package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var clearLogsCmd = &cobra.Command{
	Use:   "clear-logs",
	Short: "Delete all local bo log files under ~/.bo/log/",
	Long:  `Delete every daily log file stored under ~/.bo/log/.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("could not determine home directory: %w", err)
		}
		logDir := filepath.Join(home, ".bo", "log")

		entries, err := os.ReadDir(logDir)
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stdout, "No log directory found — nothing to delete.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading log directory: %w", err)
		}

		var files []string
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(logDir, e.Name()))
			}
		}
		if len(files) == 0 {
			fmt.Fprintln(os.Stdout, "No log files found — nothing to delete.")
			return nil
		}

		if !skipConfirm {
			fmt.Fprintf(os.Stdout, "This will delete %d log file(s) under %s.\n", len(files), logDir)
			fmt.Fprint(os.Stderr, "Proceed? [y/N] ")
			text, ok := readLine(cmd.Context())
			if !ok || strings.ToLower(strings.TrimSpace(text)) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
		}

		var failed int
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", filepath.Base(f), err)
				failed++
			}
		}
		deleted := len(files) - failed
		if deleted > 0 {
			fmt.Fprintf(os.Stdout, "Deleted %d log file(s).\n", deleted)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(clearLogsCmd)
	clearLogsCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
