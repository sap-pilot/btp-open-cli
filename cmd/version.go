package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version info",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("bo v0.1 - built 2026-05-16 22:40:17 PST")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  login           Authenticate against SAP BTP CF regions (password, SSO, CI/CD)")
		fmt.Println("  logoff          Clear stored tokens (regions preserved)")
		fmt.Println("  org-users       List users across all CF organizations with roles")
		fmt.Println("  org-space-users List users at org and space level with roles")
		fmt.Println()
		fmt.Println("Run 'bo <command> --help' for usage details.")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
