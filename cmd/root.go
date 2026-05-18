package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "bo",
	Short: "Open CLI for SAP BTP",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionString() + " — Open CLI for SAP BTP")
		fmt.Println()
		cmd.Usage()
	},
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		verbose, _ := cmd.Flags().GetBool("verbose")
		if verbose {
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
		return nil
	},
}

func Execute() {
	teardown := initFileLog()
	err := rootCmd.Execute()
	teardown()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose/debug output")
}
