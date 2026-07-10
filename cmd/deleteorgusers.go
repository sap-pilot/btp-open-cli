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
	Use:   "delete-org-space-users <org-space-users.csv>",
	Short: "Remove users from specific orgs and spaces from a CSV file",
	Long: `Remove users from CF organizations and spaces from a CSV file.

Two CSV formats are accepted:

  Simple 3-column format (broadcast — no targeting info):
    cfuser_name,cfuser_origin,cfuser_roles

  Full 9-column format (produced by "bo org-space-users --format csv"):
    region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles

Broadcast semantics (empty fields in the CSV):
  - Empty region    → target all active CF regions stored in credentials.
  - Empty org_id    → discover and target all accessible CF orgs.
  - Empty space_id  → if cfuser_roles contains space_* roles, remove those roles
                       from ALL spaces in each resolved org (or spaces matching
                       space_name if that column is non-empty).
  - Non-empty cfuser_id  → use the stored GUID directly; skip CF user lookup.

The cfuser_roles column drives scope targeting: organization_* roles → org-level
deletion; space_* roles → space-level deletion. All roles the user holds at each
targeted scope are removed (not only those listed in cfuser_roles).

All space-level removals are performed first. If there are also org-level rows, a
5-second pause follows (to allow CF's async role processing to settle) before org
roles are removed.

Use --regions to restrict processing to rows whose region column matches one of
the given shorthands (e.g. us10,eu20). When omitted, all rows are processed.

Without -y, a TOON preview of the targeted users and scopes is shown and
confirmation is required before any changes are made.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		rows, err := parseOrgSpaceUsersCSV(args[0])
		if err != nil {
			return fmt.Errorf("invalid users CSV: %w", err)
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

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

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

		// Expand source rows into concrete execution rows.
		// Broadcast semantics: empty region/org_id/space_id triggers CF discovery.
		var activeRows []orgSpaceUserRow
		for _, srcRow := range rows {
			// Split roles by level to determine which scopes to target.
			var hasOrgRoles, hasSpaceRoles bool
			for _, role := range srcRow.Roles {
				if strings.HasPrefix(role, "organization_") {
					hasOrgRoles = true
				} else if strings.HasPrefix(role, "space_") {
					hasSpaceRoles = true
				}
			}
			// If no recognized roles, default to targeting both org and space levels
			// (safe for rows that have no cfuser_roles column, e.g. a delete CSV
			// that only lists users without role detail).
			if !hasOrgRoles && !hasSpaceRoles {
				hasOrgRoles = true
				hasSpaceRoles = true
			}

			apiURLs := targetAPIURLs(srcRow.Region, creds, regionsFilter)
			for _, apiURL := range apiURLs {
				tok := creds.Tokens[apiURL]
				client := getClient(apiURL, tok)
				region := store.APIURLToRegion(apiURL)

				// ── Case 1: specific space (SpaceID non-empty, space roles present) ──
				if hasSpaceRoles && srcRow.SpaceID != "" {
					activeRows = append(activeRows, orgSpaceUserRow{
						Region:    region,
						OrgID:     srcRow.OrgID,
						OrgName:   srcRow.OrgName,
						SpaceID:   srcRow.SpaceID,
						SpaceName: srcRow.SpaceName,
						UserID:    srcRow.UserID,
						UserName:  srcRow.UserName,
						Origin:    srcRow.Origin,
						Roles:     srcRow.Roles,
					})
				}

				// ── Case 2: org-level roles OR broadcast-space rows ──
				if !hasOrgRoles && !(hasSpaceRoles && srcRow.SpaceID == "") {
					continue
				}

				// Determine target orgs.
				var targetOrgs []cf.Organization
				if srcRow.OrgID != "" {
					targetOrgs = []cf.Organization{{GUID: srcRow.OrgID, Name: srcRow.OrgName}}
				} else {
					orgs, listErr := client.ListOrganizations(ctx)
					if listErr != nil {
						fmt.Fprintf(os.Stderr, "warning: could not list orgs for %s: %v\n", apiURL, listErr)
						continue
					}
					targetOrgs = orgs
				}

				for _, org := range targetOrgs {
					// Org-level deletion row.
					if hasOrgRoles {
						activeRows = append(activeRows, orgSpaceUserRow{
							Region:   region,
							OrgID:    org.GUID,
							OrgName:  org.Name,
							SpaceID:  "",
							UserID:   srcRow.UserID,
							UserName: srcRow.UserName,
							Origin:   srcRow.Origin,
							Roles:    srcRow.Roles,
						})
					}

					// Space-level deletion rows (broadcast space targeting).
					if hasSpaceRoles && srcRow.SpaceID == "" {
						spaces, spErr := client.ListOrganizationSpaces(ctx, org.GUID)
						if spErr != nil {
							fmt.Fprintf(os.Stderr, "warning: could not list spaces for org %s: %v\n", org.Name, spErr)
							continue
						}
						for _, sp := range spaces {
							if srcRow.SpaceName != "" && !strings.EqualFold(sp.Name, srcRow.SpaceName) {
								continue
							}
							activeRows = append(activeRows, orgSpaceUserRow{
								Region:    region,
								OrgID:     org.GUID,
								OrgName:   org.Name,
								SpaceID:   sp.GUID,
								SpaceName: sp.Name,
								UserID:    srcRow.UserID,
								UserName:  srcRow.UserName,
								Origin:    srcRow.Origin,
								Roles:     srcRow.Roles,
							})
						}
					}
				}
			}
		}

		if len(activeRows) == 0 {
			return fmt.Errorf("no rows to process after filtering (check --regions or login)")
		}

		// Preview and confirm.
		if !skipConfirm {
			if err := printCosPreview("Roles to be removed:", buildCosPreviewDoc(activeRows)); err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, "Proceed with role deletion? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Separate expanded rows into space-level and org-level.
		var spaceRows, orgRows []orgSpaceUserRow
		for _, row := range activeRows {
			if row.SpaceID != "" {
				spaceRows = append(spaceRows, row)
			} else {
				orgRows = append(orgRows, row)
			}
		}

		// resolveCfUser returns the CF user GUID for a row.
		// If the row has a pre-fetched UserID, it is used directly (skipping a CF API call).
		resolveCfUser := func(row orgSpaceUserRow, client *cf.Client) (*cf.CfUser, error) {
			if row.UserID != "" {
				return &cf.CfUser{GUID: row.UserID, Username: row.UserName, Origin: row.Origin}, nil
			}
			return client.FindCfUser(ctx, row.UserName, row.Origin)
		}

		// Phase 3a: delete all space-level roles.
		if len(spaceRows) > 0 {
			fmt.Fprintln(os.Stdout, "Deleting space roles...")
			for _, row := range spaceRows {
				apiURL, tok, _ := rowAPIURL(row.Region, creds)
				client := getClient(apiURL, tok)

				cfUser, userErr := resolveCfUser(row, client)
				if userErr != nil {
					slog.Debug("user not found in CF", "user", row.UserName, "region", row.Region, "err", userErr)
					continue
				}
				roles, listErr := client.ListSpaceUserRoles(ctx, row.SpaceID, cfUser.GUID)
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "  ! %s / %s / %s: could not list space roles: %v\n",
						row.UserName, row.OrgName, row.SpaceName, listErr)
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
				apiURL, tok, _ := rowAPIURL(row.Region, creds)
				client := getClient(apiURL, tok)

				cfUser, userErr := resolveCfUser(row, client)
				if userErr != nil {
					slog.Debug("user not found in CF", "user", row.UserName, "region", row.Region, "err", userErr)
					continue
				}
				roles, listErr := client.ListOrganizationUserRoles(ctx, row.OrgID, cfUser.GUID)
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "  ! %s / %s: could not list org roles: %v\n",
						row.UserName, row.OrgName, listErr)
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
	deleteOrgSpaceUsersCmd.Flags().String("regions", "", "Only process rows whose region column matches one of these shorthands (e.g. us10,eu10); for broadcast rows (empty region), restricts which active regions are targeted")
	deleteOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
