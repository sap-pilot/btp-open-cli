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

// RegisterCommand registers a custom command with the root command.
// Call this from init() in your cmd/custom package to add project-specific
// commands without touching any upstream file.
func RegisterCommand(c *cobra.Command) {
	rootCmd.AddCommand(c)
}

func init() {
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose/debug output")

	rootCmd.AddGroup(
		&cobra.Group{ID: "common", Title: "Common:"},
		&cobra.Group{ID: "cf-org", Title: "CF Org:"},
		&cobra.Group{ID: "xsuaa", Title: "XSUAA Users:"},
		&cobra.Group{ID: "destination", Title: "Destination:"},
		&cobra.Group{ID: "subaccount", Title: "Subaccount Automation:"},
		&cobra.Group{ID: "utilities", Title: "Utilities:"},
	)
	rootCmd.SetHelpCommandGroupID("utilities")
	rootCmd.SetCompletionCommandGroupID("utilities")
}
