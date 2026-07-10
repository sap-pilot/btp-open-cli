package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// deleteXsuaaUser identifies a user to delete by region, org GUID, and XSUAA user ID.
type deleteXsuaaUser struct {
	Region string
	OrgID  string
	UserID string
}

// parseDeleteXsuaaUsersCSV reads a CSV that must contain at least the columns
// region, org_id, and user_id (in any position). Extra columns are ignored,
// making the output of "bo users --format csv" directly usable as input.
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

	colIdx := map[string]int{}
	for i, h := range header {
		colIdx[strings.TrimSpace(h)] = i
	}
	regionIdx, hasRegion := colIdx["region"]
	orgIdx, hasOrg := colIdx["org_id"]
	userIdx, hasUser := colIdx["user_id"]
	if !hasRegion || !hasOrg || !hasUser {
		return nil, fmt.Errorf("invalid header — required columns: region, org_id, user_id")
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
		maxIdx := regionIdx
		if orgIdx > maxIdx {
			maxIdx = orgIdx
		}
		if userIdx > maxIdx {
			maxIdx = userIdx
		}
		if len(row) <= maxIdx {
			return nil, fmt.Errorf("line %d: too few columns", line)
		}
		region := strings.TrimSpace(row[regionIdx])
		orgID := strings.TrimSpace(row[orgIdx])
		userID := strings.TrimSpace(row[userIdx])
		if region == "" || orgID == "" || userID == "" {
			return nil, fmt.Errorf("line %d: region, org_id, and user_id cannot be empty", line)
		}
		users = append(users, deleteXsuaaUser{Region: region, OrgID: orgID, UserID: userID})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}

// ── command ───────────────────────────────────────────────────────────────────

var deleteUsersCmd = &cobra.Command{
	Use:   "delete-users <users.csv>",
	Short: "Delete XSUAA users across all accessible organizations",
	Long: `Delete users from the XSUAA (Authorization and Trust Management) apiaccess service.

The CSV argument must contain at least the columns: region, org_id, user_id

Extra columns are ignored, so the output of "bo users --format csv" can be passed
directly:

  bo users --format csv > users.csv
  bo delete-users users.csv

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually.

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

Without -y, a TOON preview of all users that will be deleted is shown before
execution and confirmation is required.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		skipPattern, _ := cmd.Flags().GetString("skip")

		csvUsers, err := parseDeleteXsuaaUsersCSV(args[0])
		if err != nil {
			return fmt.Errorf("invalid users CSV: %w", err)
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		// Derive CF API URLs from the unique regions referenced in the CSV.
		apiURLSet := make(map[string]bool)
		for _, u := range csvUsers {
			apiURLSet[store.RegionToAPIURL(u.Region)] = true
		}
		apiURLs := make([]string, 0, len(apiURLSet))
		for url := range apiURLSet {
			apiURLs = append(apiURLs, url)
		}

		// Build an org filter from the CSV so XSUAA tokens are only resolved
		// for orgs that actually appear in the input.
		orgFilter := make(cosOrgSet, 0, len(csvUsers))
		seen := make(map[string]bool)
		for _, u := range csvUsers {
			if !seen[u.OrgID] {
				orgFilter = append(orgFilter, cosOrgRef{ID: u.OrgID})
				seen[u.OrgID] = true
			}
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Phase 1: resolve XSUAA tokens for the orgs referenced in the CSV.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, orgFilter, nil, false)
		if err != nil {
			return err
		}

		// Phase 2: match each client (org) to the CSV rows that target it.
		type deleteTarget struct {
			regionName string
			orgGUID    string
			orgName    string
			apiURL     string
			token      string
			userID     string
		}
		var targets []deleteTarget
		for _, w := range clients {
			for _, u := range csvUsers {
				if !strings.EqualFold(u.OrgID, w.OrgGUID) {
					continue
				}
				if !strings.EqualFold(u.Region, w.RegionName) {
					continue
				}
				targets = append(targets, deleteTarget{
					regionName: w.RegionName,
					orgGUID:    w.OrgGUID,
					orgName:    w.OrgName,
					apiURL:     w.APIURL,
					token:      w.Token,
					userID:     u.UserID,
				})
			}
		}

		if len(targets) == 0 {
			fmt.Fprintln(os.Stdout, "No matching users found.")
			return nil
		}

		// Phase 3: fetch full user attributes from XSUAA for the preview.
		// Index users by (apiURL, userID) so we can populate all fields.
		type orgKey struct{ apiURL, orgGUID string }
		userAttrs := make(map[string]xsuaa.User) // key: userID
		fetchedOrgs := make(map[orgKey]bool)
		for _, t := range targets {
			ok := fetchedOrgs[orgKey{t.apiURL, t.orgGUID}]
			if ok {
				continue
			}
			fetchedOrgs[orgKey{t.apiURL, t.orgGUID}] = true
			users, fetchErr := xsuaa.ListUsers(ctx, t.apiURL, t.token)
			if fetchErr != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] %s: could not fetch user list: %v\n",
					t.regionName, t.orgName, fetchErr)
				continue
			}
			for _, u := range users {
				userAttrs[u.ID] = u
			}
		}

		// Apply --skip filter using fetched user attributes.
		if skipPattern != "" {
			var filtered []deleteTarget
			for _, t := range targets {
				u := userAttrs[t.userID]
				if skipMatches(skipPattern, t.userID, u.UserName, xsuaa.PrimaryEmail(u.Emails), xsuaa.GroupValues(u.Groups)) {
					continue
				}
				filtered = append(filtered, t)
			}
			targets = filtered
		}
		if len(targets) == 0 {
			fmt.Fprintln(os.Stdout, "No users to delete after applying --skip.")
			return nil
		}

		// Phase 4: assemble preview, grouped by region then org.
		regionOrder := make([]string, 0)
		regionSeen := make(map[string]bool)
		for _, t := range targets {
			if !regionSeen[t.regionName] {
				regionOrder = append(regionOrder, t.regionName)
				regionSeen[t.regionName] = true
			}
		}

		regionOrgs := make(map[string]map[string]*usrOutOrg)
		for _, t := range targets {
			if regionOrgs[t.regionName] == nil {
				regionOrgs[t.regionName] = make(map[string]*usrOutOrg)
			}
			if regionOrgs[t.regionName][t.orgGUID] == nil {
				regionOrgs[t.regionName][t.orgGUID] = &usrOutOrg{ID: t.orgGUID, Name: t.orgName}
			}
			u := userAttrs[t.userID]
			regionOrgs[t.regionName][t.orgGUID].Users = append(
				regionOrgs[t.regionName][t.orgGUID].Users,
				usrOutUser{
					ID:            t.userID,
					ExternalID:    u.ExternalID,
					Origin:        u.Origin,
					UserName:      u.UserName,
					Email:         xsuaa.PrimaryEmail(u.Emails),
					LastLogonTime: xsuaa.MSToISO(u.LastLogonTime),
					Groups:        xsuaa.GroupValues(u.Groups),
				},
			)
		}

		var previewRegions []usrOutRegion
		for _, rid := range regionOrder {
			var orgs []usrOutOrg
			for _, org := range regionOrgs[rid] {
				orgs = append(orgs, *org)
			}
			previewRegions = append(previewRegions, usrOutRegion{ID: rid, Orgs: orgs})
		}

		if !skipConfirm {
			out, err := toonenc.Marshal(usrOutDoc{Regions: previewRegions}, toonenc.WithIndent(2))
			if err != nil {
				return fmt.Errorf("encoding preview: %w", err)
			}
			fmt.Fprintln(os.Stdout, "Users to be deleted:")
			os.Stdout.Write(out)
			fmt.Fprintln(os.Stdout)
			fmt.Fprint(os.Stdout, "Proceed with user deletion? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Phase 4: delete users by ID.
		fmt.Fprintln(os.Stdout, "Deleting users...")
		for _, t := range targets {
			if err := xsuaa.DeleteUser(ctx, t.apiURL, t.token, t.userID); err != nil {
				fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: %v\n",
					t.regionName, t.orgName, t.userID, err)
			} else {
				fmt.Fprintf(os.Stdout, "  - [%s] %s / %s\n",
					t.regionName, t.orgName, t.userID)
			}
		}
		return nil
	},
}

func init() {
	deleteUsersCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(deleteUsersCmd)
	deleteUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt for user deletion")
	deleteUsersCmd.Flags().String("skip", "", "Skip users whose user_id, user_name, email, or groups contain this pattern (case-insensitive substring match)")
}
