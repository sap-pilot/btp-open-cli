package cmd

import (
	"fmt"
	"os"

	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var logoffCmd = &cobra.Command{
	Use:   "logoff",
	Short: "Clear local credentials and log off",
	Long:  `Remove the locally stored access token, effectively logging off.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := store.ClearTokens(); err != nil {
			return fmt.Errorf("clearing credentials: %w", err)
		}
		fmt.Fprintln(os.Stdout, "Logged off. Run 'bo login' to authenticate again (regions preserved).")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(logoffCmd)
}
