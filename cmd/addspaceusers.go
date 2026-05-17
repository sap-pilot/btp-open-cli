package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var addSpaceUsersCmd = &cobra.Command{
	Use:   "add-space-users",
	Short: "Add users to all spaces across every accessible CF organization",
	Long: `Add users to every space in every CF organization across one or more regions.

The CSV file must have the header: name,origin,roles
Roles are semicolon-separated (e.g. space_developer;space_manager).

Users are created via POST /v3/users and roles via POST /v3/roles.
Both operations are idempotent — existing users/roles are left unchanged.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		filePath, _ := cmd.Flags().GetString("file")

		users, err := parseUsersCSV(filePath)
		if err != nil {
			return fmt.Errorf("invalid CSV: %w", err)
		}
		fmt.Fprintf(os.Stdout, "Loaded %d user(s) from %s\n\n", len(users), filePath)

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		var apiURLs []string
		if regionsFlag != "" {
			for _, r := range splitCSV(regionsFlag) {
				apiURLs = append(apiURLs, store.RegionToAPIURL(r))
			}
		} else {
			apiURLs = creds.ActiveAPIURLs
		}
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, apiURL := range apiURLs {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] no token — run: bo login --regions %s\n", regionName, regionName)
					mu.Unlock()
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, err)
					mu.Unlock()
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				for _, org := range orgs {
					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: error listing spaces: %v\n", regionName, org.Name, err)
						mu.Unlock()
						continue
					}
					slog.Debug("spaces fetched", "region", regionName, "org", org.Name, "count", len(spaces))

					for _, space := range spaces {
						mu.Lock()
						fmt.Fprintf(os.Stdout, "[%s] %s / %s:\n", regionName, org.Name, space.Name)
						mu.Unlock()

						for _, u := range users {
							var addedRoles, failedRoles []string
							for _, role := range u.Roles {
								if err := client.CreateSpaceRole(ctx, role, u.Name, u.Origin, space.GUID); err != nil {
									failedRoles = append(failedRoles, role)
									slog.Debug("role error", "user", u.Name, "role", role, "err", err)
								} else {
									addedRoles = append(addedRoles, role)
								}
							}

							mu.Lock()
							fmt.Fprintf(os.Stdout, "  + %s [%s]\n", u.Name, strings.Join(addedRoles, ", "))
							if len(failedRoles) > 0 {
								fmt.Fprintf(os.Stderr, "  ! %s: failed roles: %s\n", u.Name, strings.Join(failedRoles, ", "))
							}
							mu.Unlock()
						}
					}
				}
			}(apiURL)
		}
		wg.Wait()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(addSpaceUsersCmd)
	addSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	addSpaceUsersCmd.Flags().String("file", "", "Path to the CSV file (required; columns: name,origin,roles)")
	addSpaceUsersCmd.MarkFlagRequired("file")
}
