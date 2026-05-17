package cmd

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

//go:embed version.txt
var versionFile string

// versionInfo is the trimmed content of version.txt, available package-wide.
var versionInfo = strings.TrimSpace(versionFile)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version info",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionInfo)
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
