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

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// deleteXsuaaUser identifies a user to delete by origin and userName.
type deleteXsuaaUser struct {
	Origin   string
	UserName string
}

// parseDeleteXsuaaUsersCSV reads a CSV with header "origin,userName".
func parseDeleteXsuaaUsersCSV(path string) ([]deleteXsuaaUser, error) {
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
	if len(header) < 2 || header[0] != "origin" || header[1] != "userName" {
		return nil, fmt.Errorf("invalid header — expected: origin,userName")
	}

	var users []deleteXsuaaUser
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
		origin := strings.TrimSpace(row[0])
		userName := strings.TrimSpace(row[1])
		if origin == "" || userName == "" {
			return nil, fmt.Errorf("line %d: origin and userName cannot be empty", line)
		}
		users = append(users, deleteXsuaaUser{Origin: origin, UserName: userName})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}

// ── command ───────────────────────────────────────────────────────────────────

var deleteUsersCmd = &cobra.Command{
	Use:   "delete-users",
	Short: "Delete XSUAA users across all accessible organizations",
	Long: `Delete users from the XSUAA (Authorization and Trust Management) apiaccess service
across one or more regions and organizations.

The --users CSV must have the header: origin,userName

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually (suppress with
--no-prompt to skip the org silently instead).

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

Without -y, a TOON preview of all users that will be deleted is shown before execution,
and confirmation is required.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile, _ := cmd.Flags().GetString("users")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")

		csvUsers, err := parseDeleteXsuaaUsersCSV(usersFile)
		if err != nil {
			return fmt.Errorf("invalid --users CSV: %w", err)
		}

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

		var includeOrgs cosOrgSet
		if orgsFile != "" {
			includeOrgs, err = parseCosOrgCSV(orgsFile)
			if err != nil {
				return fmt.Errorf("invalid --orgs CSV: %w", err)
			}
		}

		var excludeOrgs cosOrgSet
		if excludeOrgsFile != "" {
			excludeOrgs, err = parseCosOrgCSV(excludeOrgsFile)
			if err != nil {
				return fmt.Errorf("invalid --excludeOrgs CSV: %w", err)
			}
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Phase 1: resolve XSUAA tokens for all accessible orgs.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
		if err != nil {
			return err
		}

		// Phase 2: fetch XSUAA users for each org in parallel and filter
		// down to the users listed in the CSV.
		type orgResult struct {
			regionName string
			orgGUID    string
			orgName    string
			apiURL     string
			token      string
			matched    []xsuaa.User
			err        error
		}
		results := make([]orgResult, len(clients))
		var wg sync.WaitGroup

		for i, w := range clients {
			wg.Add(1)
			go func(idx int, w xsuaaOrgClient) {
				defer wg.Done()
				slog.Debug("fetching XSUAA users for deletion", "region", w.RegionName, "org", w.OrgName)

				allUsers, err := xsuaa.ListUsers(ctx, w.APIURL, w.Token)
				if err != nil {
					results[idx] = orgResult{regionName: w.RegionName, orgGUID: w.OrgGUID, orgName: w.OrgName, err: err}
					return
				}

				var matched []xsuaa.User
				for _, u := range allUsers {
					for _, cu := range csvUsers {
						if strings.EqualFold(u.Origin, cu.Origin) && strings.EqualFold(u.UserName, cu.UserName) {
							matched = append(matched, u)
							break
						}
					}
				}
				results[idx] = orgResult{
					regionName: w.RegionName,
					orgGUID:    w.OrgGUID,
					orgName:    w.OrgName,
					apiURL:     w.APIURL,
					token:      w.Token,
					matched:    matched,
				}
			}(i, w)
		}
		wg.Wait()

		// Phase 3: assemble preview, preserving region order from clients.
		regionOrder := make([]string, 0)
		regionSeen := make(map[string]bool)
		for _, c := range clients {
			if !regionSeen[c.RegionName] {
				regionOrder = append(regionOrder, c.RegionName)
				regionSeen[c.RegionName] = true
			}
		}

		regionOrgs := make(map[string][]usrOutOrg)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.orgName, r.err)
				continue
			}
			if len(r.matched) == 0 {
				continue
			}
			var outUsers []usrOutUser
			for _, u := range r.matched {
				outUsers = append(outUsers, usrOutUser{
					ID:            u.ID,
					ExternalID:    u.ExternalID,
					Origin:        u.Origin,
					UserName:      u.UserName,
					Email:         xsuaa.PrimaryEmail(u.Emails),
					LastLogonTime: xsuaa.MSToISO(u.LastLogonTime),
					Groups:        xsuaa.GroupValues(u.Groups),
				})
			}
			regionOrgs[r.regionName] = append(regionOrgs[r.regionName], usrOutOrg{
				ID:    r.orgGUID,
				Name:  r.orgName,
				Users: outUsers,
			})
		}

		var previewRegions []usrOutRegion
		for _, rid := range regionOrder {
			if orgs := regionOrgs[rid]; len(orgs) > 0 {
				previewRegions = append(previewRegions, usrOutRegion{ID: rid, Orgs: orgs})
			}
		}

		if len(previewRegions) == 0 {
			fmt.Fprintln(os.Stdout, "No matching users found.")
			return nil
		}

		if !skipConfirm {
			out, err := toonenc.Marshal(usrOutDoc{Regions: previewRegions}, toonenc.WithIndent(2))
			if err != nil {
				return fmt.Errorf("encoding preview: %w", err)
			}
			fmt.Fprintln(os.Stdout, "Users to be deleted:")
			os.Stdout.Write(out)
			fmt.Fprintln(os.Stdout)
			fmt.Fprint(os.Stderr, "Proceed with user deletion? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Phase 4: delete matched users sequentially per org.
		fmt.Fprintln(os.Stdout, "Deleting users...")
		for _, r := range results {
			if r.err != nil || len(r.matched) == 0 {
				continue
			}
			for _, u := range r.matched {
				if err := xsuaa.DeleteUser(ctx, r.apiURL, r.token, u.ID); err != nil {
					fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s (%s): %v\n",
						r.regionName, r.orgName, u.UserName, u.Origin, err)
				} else {
					fmt.Fprintf(os.Stdout, "  - [%s] %s / %s (%s)\n",
						r.regionName, r.orgName, u.UserName, u.Origin)
				}
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deleteUsersCmd)
	deleteUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteUsersCmd.Flags().String("users", "", "Path to CSV file of users to delete (required; columns: origin,userName)")
	deleteUsersCmd.MarkFlagRequired("users")
	deleteUsersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	deleteUsersCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	deleteUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt for user deletion")
	deleteUsersCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
}
