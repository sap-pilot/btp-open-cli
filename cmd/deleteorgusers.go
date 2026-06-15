package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var deleteOrgSpaceUsersCmd = &cobra.Command{
	Use:   "delete-org-space-users",
	Short: "Remove users from specific orgs and spaces from an org-space-users CSV",
	Long: `Remove users from CF organizations and spaces from a targeted CSV file.

The --users CSV must use the format produced by "bo org-space-users --format csv":

  region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles

Targeting rules:
  - Rows with an empty space_id → remove ALL of the user's roles from that org.
  - Rows with a non-empty space_id → remove ALL of the user's roles from that space.

All space-level removals are performed first. If there are also org-level rows, a
5-second pause follows (to allow CF's async role processing to settle) before org
roles are removed.

Use --regions to restrict processing to rows whose region column matches one of
the given shorthands (e.g. us10,eu20). When omitted, all rows are processed.

Without -y, a TOON preview of the targeted users and scopes is shown and
confirmation is required before any changes are made.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile, _ := cmd.Flags().GetString("users")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		rows, err := parseOrgSpaceUsersCSV(usersFile)
		if err != nil {
			return fmt.Errorf("invalid --users CSV: %w", err)
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		// Build region filter from --regions if provided.
		var regionsFilter map[string]bool
		if regionsFlag != "" {
			regionsFilter = make(map[string]bool)
			for _, r := range splitCSV(regionsFlag) {
				regionsFilter[strings.TrimSpace(r)] = true
			}
		}

		// Filter rows: apply region filter and check token availability.
		noTokenWarnedRegions := map[string]bool{}
		var activeRows []orgSpaceUserRow
		for _, row := range rows {
			if regionsFilter != nil && !regionsFilter[row.Region] {
				continue
			}
			if _, _, ok := rowAPIURL(row.Region, creds); !ok {
				if !noTokenWarnedRegions[row.Region] {
					noTokenWarnedRegions[row.Region] = true
					fmt.Fprintf(os.Stderr, "warning: no token for region %q — run: bo login --regions %s\n", row.Region, row.Region)
				}
				continue
			}
			activeRows = append(activeRows, row)
		}
		if len(activeRows) == 0 {
			return fmt.Errorf("no rows to process after filtering (check --regions or login)")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Preview and confirm.
		if !skipConfirm {
			if err := printCosPreview("Roles to be removed:", buildCosPreviewDoc(activeRows)); err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Proceed with role deletion? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Separate into space-level and org-level rows.
		var spaceRows, orgRows []orgSpaceUserRow
		for _, row := range activeRows {
			if row.SpaceID != "" {
				spaceRows = append(spaceRows, row)
			} else {
				orgRows = append(orgRows, row)
			}
		}

		// Create one CF client per API URL (lazily).
		clients := map[string]*cf.Client{}
		getClient := func(apiURL string, tok store.RegionToken) *cf.Client {
			if c, ok := clients[apiURL]; ok {
				return c
			}
			c := cf.NewClient(apiURL, tok.AccessToken)
			c.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
			clients[apiURL] = c
			return c
		}

		// Phase 3a: delete all space-level roles.
		if len(spaceRows) > 0 {
			fmt.Fprintln(os.Stdout, "Deleting space roles...")
			for _, row := range spaceRows {
				apiURL, tok, _ := rowAPIURL(row.Region, creds) // verified above
				client := getClient(apiURL, tok)

				cfUser, err := client.FindCfUser(ctx, row.UserName, row.Origin)
				if err != nil {
					slog.Debug("user not found in CF", "user", row.UserName, "region", row.Region, "err", err)
					continue
				}
				roles, err := client.ListSpaceUserRoles(ctx, row.SpaceID, cfUser.GUID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ! %s / %s / %s: could not list space roles: %v\n",
						row.UserName, row.OrgName, row.SpaceName, err)
					continue
				}
				var removed, failed []string
				for _, role := range roles {
					if err := client.DeleteRole(ctx, role.GUID); err != nil {
						failed = append(failed, fmt.Sprintf("%s (%v)", role.Type, err))
					} else {
						removed = append(removed, role.Type)
					}
				}
				if len(removed) > 0 {
					fmt.Fprintf(os.Stdout, "  - %s / %s / %s [%s]\n",
						row.UserName, row.OrgName, row.SpaceName, strings.Join(removed, ", "))
				}
				for _, e := range failed {
					fmt.Fprintf(os.Stderr, "  ! %s / %s / %s: failed: %s\n",
						row.UserName, row.OrgName, row.SpaceName, e)
				}
			}
		}

		// Wait for CF async processing only when both phase sets are non-empty.
		if len(spaceRows) > 0 && len(orgRows) > 0 {
			fmt.Fprintln(os.Stdout, "\nWaiting 5 s for CF async processing...")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}

		// Phase 3b: delete all org-level roles.
		if len(orgRows) > 0 {
			fmt.Fprintln(os.Stdout, "Deleting org roles...")
			for _, row := range orgRows {
				apiURL, tok, _ := rowAPIURL(row.Region, creds) // verified above
				client := getClient(apiURL, tok)

				cfUser, err := client.FindCfUser(ctx, row.UserName, row.Origin)
				if err != nil {
					slog.Debug("user not found in CF", "user", row.UserName, "region", row.Region, "err", err)
					continue
				}
				roles, err := client.ListOrganizationUserRoles(ctx, row.OrgID, cfUser.GUID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ! %s / %s: could not list org roles: %v\n",
						row.UserName, row.OrgName, err)
					continue
				}
				var removed, failed []string
				for _, role := range roles {
					if err := client.DeleteRole(ctx, role.GUID); err != nil {
						failed = append(failed, fmt.Sprintf("%s (%v)", role.Type, err))
					} else {
						removed = append(removed, role.Type)
					}
				}
				if len(removed) > 0 {
					fmt.Fprintf(os.Stdout, "  - %s / %s [%s]\n",
						row.UserName, row.OrgName, strings.Join(removed, ", "))
				}
				for _, e := range failed {
					fmt.Fprintf(os.Stderr, "  ! %s / %s: failed: %s\n",
						row.UserName, row.OrgName, e)
				}
			}
		}
		return nil
	},
}

func init() {
	deleteOrgSpaceUsersCmd.GroupID = "cf-org"
	rootCmd.AddCommand(deleteOrgSpaceUsersCmd)
	deleteOrgSpaceUsersCmd.Flags().String("regions", "", "Only process rows whose region column matches one of these shorthands (e.g. us10,eu10)")
	deleteOrgSpaceUsersCmd.Flags().String("users", "", "Path to org-space-users CSV file (required; columns: region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles)")
	deleteOrgSpaceUsersCmd.MarkFlagRequired("users")
	deleteOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
