package cmd

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var orgUsersCmd = &cobra.Command{
	Use:   "org-users",
	Short: "List all org users across every accessible organization",
	Long: `List all users in every Cloud Foundry organization the current user has
access to. Output is CSV with columns: org_id,org_name,user_id,user_name,user_origin`,
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --sso --region <region>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		client := cf.NewClient(token.APIURL, token.AccessToken)

		slog.Debug("fetching organizations")
		orgs, err := client.ListOrganizations(ctx)
		if err != nil {
			return fmt.Errorf("listing organizations: %w", err)
		}
		slog.Debug("organizations fetched", "count", len(orgs))

		w := csv.NewWriter(os.Stdout)
		defer w.Flush()

		if err := w.Write([]string{"org_id", "org_name", "user_id", "user_name", "user_origin"}); err != nil {
			return err
		}

		for _, org := range orgs {
			slog.Debug("fetching users", "org", org.Name, "guid", org.GUID)
			users, err := client.ListOrganizationUsers(ctx, org.GUID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not fetch users for org %q (%s): %v\n", org.Name, org.GUID, err)
				continue
			}
			for _, u := range users {
				if err := w.Write([]string{org.GUID, org.Name, u.GUID, u.Username, u.Origin}); err != nil {
					return err
				}
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(orgUsersCmd)
}
