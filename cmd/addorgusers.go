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

// csvUser represents one row from the input CSV.
type csvUser struct {
	Name   string
	Origin string
	Roles  []string
}

func parseAddOrgUsersCSV(path string) ([]csvUser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	// Read and validate header.
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	if len(header) < 3 || header[0] != "name" || header[1] != "origin" || header[2] != "roles" {
		return nil, fmt.Errorf("invalid header — expected: name,origin,roles")
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
		if len(row) < 3 {
			return nil, fmt.Errorf("line %d: expected 3 columns, got %d", line, len(row))
		}
		name := strings.TrimSpace(row[0])
		origin := strings.TrimSpace(row[1])
		if name == "" || origin == "" {
			return nil, fmt.Errorf("line %d: name and origin cannot be empty", line)
		}
		var roles []string
		for _, r := range strings.Split(row[2], ";") {
			if v := strings.TrimSpace(r); v != "" {
				roles = append(roles, v)
			}
		}
		users = append(users, csvUser{Name: name, Origin: origin, Roles: roles})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}

var addOrgUsersCmd = &cobra.Command{
	Use:   "add-org-users",
	Short: "Add users to all accessible CF organizations from a CSV file",
	Long: `Add users to every CF organization in one or more regions.

The CSV file must have the header: name,origin,roles
Roles are semicolon-separated (e.g. organization_user;organization_manager).

Users are created via POST /v3/users and roles via POST /v3/roles.
Both operations are idempotent — existing users/roles are left unchanged.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		filePath, _ := cmd.Flags().GetString("file")

		users, err := parseAddOrgUsersCSV(filePath)
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

					for _, u := range users {
						cfUser, err := client.CreateUser(ctx, u.Name, u.Origin)
						if err != nil {
							mu.Lock()
							fmt.Fprintf(os.Stderr, "  ! %s: failed to resolve user: %v\n", u.Name, err)
							mu.Unlock()
							continue
						}

						var addedRoles, failedRoles []string
						for _, role := range u.Roles {
							if err := client.CreateOrganizationRole(ctx, role, cfUser.GUID, org.GUID); err != nil {
								failedRoles = append(failedRoles, role)
								slog.Debug("role error", "user", u.Name, "role", role, "err", err)
							} else {
								addedRoles = append(addedRoles, role)
							}
						}

						mu.Lock()
						if len(failedRoles) == 0 {
							fmt.Fprintf(os.Stdout, "  + %s [%s]\n", u.Name, strings.Join(addedRoles, ", "))
						} else {
							fmt.Fprintf(os.Stdout, "  + %s [%s]", u.Name, strings.Join(addedRoles, ", "))
							fmt.Fprintf(os.Stderr, "  ! %s: failed roles: %s\n", u.Name, strings.Join(failedRoles, ", "))
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
	rootCmd.AddCommand(addOrgUsersCmd)
	addOrgUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	addOrgUsersCmd.Flags().String("file", "", "Path to the CSV file (required; columns: name,origin,roles)")
	addOrgUsersCmd.MarkFlagRequired("file")
}
