package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Build-time variables — overridden via -ldflags during release builds.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// versionString returns the full version line shown to users.
func versionString() string {
	return fmt.Sprintf("bo %s+%s.%s", Version, Commit, Date)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version info",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionString())
	},
}

func init() {
	versionCmd.GroupID = "common"
	rootCmd.AddCommand(versionCmd)
}
