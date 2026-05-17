package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// parseDeleteUsersCSV parses a CSV with header "name,origin" and returns the
// list of users to delete. Shared by delete-org-users and delete-space-users.
func parseDeleteUsersCSV(path string) ([]csvUser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	if len(header) < 2 || header[0] != "name" || header[1] != "origin" {
		return nil, fmt.Errorf("invalid header — expected: name,origin")
	}

	var users []csvUser
	for line := 2; ; line++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(row) < 2 {
			return nil, fmt.Errorf("line %d: expected 2 columns, got %d", line, len(row))
		}
		name := strings.TrimSpace(row[0])
		origin := strings.TrimSpace(row[1])
		if name == "" || origin == "" {
			return nil, fmt.Errorf("line %d: name and origin cannot be empty", line)
		}
		users = append(users, csvUser{Name: name, Origin: origin})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}

var deleteOrgSpaceUsersCmd = &cobra.Command{
	Use:   "delete-org-space-users",
	Short: "Remove users from all spaces and organizations across accessible CF orgs",
	Long: `Remove users from every space and organization across one or more regions.

The CSV file must have the header: name,origin

Space-level role assignments are deleted first, then org-level roles, via
DELETE /v3/roles/{guid}. Users not found or with no roles are skipped with a warning.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		filePath, _ := cmd.Flags().GetString("file")

		users, err := parseDeleteUsersCSV(filePath)
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
					mu.Lock()
					fmt.Fprintf(os.Stdout, "[%s] %s:\n", regionName, org.Name)
					mu.Unlock()

					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "  ! could not list spaces: %v\n", err)
						mu.Unlock()
						// still attempt org-level removal below
					}

					for _, u := range users {
						cfUser, err := client.FindUser(ctx, u.Name, u.Origin)
						if err != nil {
							mu.Lock()
							fmt.Fprintf(os.Stderr, "  ! %s: user not found: %v\n", u.Name, err)
							mu.Unlock()
							continue
						}

						// 1. Remove space-level roles first.
						for _, space := range spaces {
							spaceRoles, err := client.ListSpaceUserRoles(ctx, space.GUID, cfUser.GUID)
							if err != nil {
								mu.Lock()
								fmt.Fprintf(os.Stderr, "  ! %s / %s: could not list space roles: %v\n", u.Name, space.Name, err)
								mu.Unlock()
								continue
							}
							var removed []string
							var roleErrs []string
							for _, role := range spaceRoles {
								if err := client.DeleteRole(ctx, role.GUID); err != nil {
									roleErrs = append(roleErrs, fmt.Sprintf("%s (%v)", role.Type, err))
								} else {
									removed = append(removed, role.Type)
								}
							}
							if len(removed) > 0 || len(roleErrs) > 0 {
								mu.Lock()
								if len(removed) > 0 {
									fmt.Fprintf(os.Stdout, "  - %s / %s [%s]\n", u.Name, space.Name, strings.Join(removed, ", "))
								}
								for _, e := range roleErrs {
									fmt.Fprintf(os.Stderr, "  ! %s / %s: failed to delete space role: %s\n", u.Name, space.Name, e)
								}
								mu.Unlock()
							}
						}

						// 2. Remove org-level roles.
						orgRoles, err := client.ListOrganizationUserRoles(ctx, org.GUID, cfUser.GUID)
						if err != nil {
							mu.Lock()
							fmt.Fprintf(os.Stderr, "  ! %s: could not list org roles: %v\n", u.Name, err)
							mu.Unlock()
							continue
						}
						if len(orgRoles) == 0 {
							mu.Lock()
							fmt.Fprintf(os.Stdout, "  ~ %s: no org roles, skipping org removal\n", u.Name)
							mu.Unlock()
							continue
						}
						var removed []string
						var roleErrs []string
						for _, role := range orgRoles {
							if err := client.DeleteRole(ctx, role.GUID); err != nil {
								roleErrs = append(roleErrs, fmt.Sprintf("%s (%v)", role.Type, err))
							} else {
								removed = append(removed, role.Type)
							}
						}
						mu.Lock()
						if len(removed) > 0 {
							fmt.Fprintf(os.Stdout, "  - %s [%s]\n", u.Name, strings.Join(removed, ", "))
						}
						for _, e := range roleErrs {
							fmt.Fprintf(os.Stderr, "  ! %s: failed to delete org role: %s\n", u.Name, e)
						}
						mu.Unlock()
					}
				}
			}(apiURL)
		}
		wg.Wait()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deleteOrgSpaceUsersCmd)
	deleteOrgSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteOrgSpaceUsersCmd.Flags().String("file", "", "Path to the CSV file (required; columns: name,origin)")
	deleteOrgSpaceUsersCmd.MarkFlagRequired("file")
}
