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

// createXsuaaUser holds one row from the create-users CSV.
type createXsuaaUser struct {
	Region   string
	OrgID    string
	Origin   string
	UserName string
	Email    string
	Groups   []string // role collection names (semicolon-separated in CSV)
}

// parseCreateXsuaaUsersCSV reads a CSV that must contain at least the columns
// region, org_id, user_origin, user_name, email, and groups (in any position).
// Extra columns are ignored, so the output of "bo users --format csv" is accepted.
func parseCreateXsuaaUsersCSV(path string) ([]createXsuaaUser, error) {
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
	required := []string{"region", "org_id", "user_origin", "user_name", "email", "groups"}
	for _, col := range required {
		if _, ok := colIdx[col]; !ok {
			return nil, fmt.Errorf("invalid header — required columns: %s", strings.Join(required, ", "))
		}
	}

	regionIdx := colIdx["region"]
	orgIdx := colIdx["org_id"]
	originIdx := colIdx["user_origin"]
	nameIdx := colIdx["user_name"]
	emailIdx := colIdx["email"]
	groupsIdx := colIdx["groups"]

	maxIdx := regionIdx
	for _, idx := range []int{orgIdx, originIdx, nameIdx, emailIdx, groupsIdx} {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	var users []createXsuaaUser
	for line := 2; ; line++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(row) <= maxIdx {
			return nil, fmt.Errorf("line %d: too few columns", line)
		}
		region := strings.TrimSpace(row[regionIdx])
		orgID := strings.TrimSpace(row[orgIdx])
		origin := strings.TrimSpace(row[originIdx])
		userName := strings.TrimSpace(row[nameIdx])
		email := strings.TrimSpace(row[emailIdx])
		groups := strings.TrimSpace(row[groupsIdx])

		if region == "" || orgID == "" || origin == "" || userName == "" || email == "" {
			return nil, fmt.Errorf("line %d: region, org_id, user_origin, user_name, and email cannot be empty", line)
		}

		var groupList []string
		for _, g := range strings.Split(groups, ";") {
			if g = strings.TrimSpace(g); g != "" {
				groupList = append(groupList, g)
			}
		}

		users = append(users, createXsuaaUser{
			Region:   region,
			OrgID:    orgID,
			Origin:   origin,
			UserName: userName,
			Email:    email,
			Groups:   groupList,
		})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}

// ── command ───────────────────────────────────────────────────────────────────

var createUsersCmd = &cobra.Command{
	Use:   "create-users <users.csv>",
	Short: "Create XSUAA users and assign role collections",
	Long: `Create users in the XSUAA (Authorization and Trust Management) apiaccess service
and assign them to role collections (groups).

The CSV argument must contain at least the columns:
  region, org_id, user_origin, user_name, email, groups

Extra columns are ignored, so the output of "bo users --format csv" can be passed
directly. The groups column is semicolon-separated, e.g.:

  "Role A;Role B;Role C"

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually.

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

If a user already exists (HTTP 409), creation is skipped and role collection
assignment proceeds. Without -y, a TOON preview is shown and confirmation is
required before any changes are made.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		skipPattern, _ := cmd.Flags().GetString("skip")
		includePattern, _ := cmd.Flags().GetString("include")

		csvUsers, err := parseCreateXsuaaUsersCSV(args[0])
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
		type createTarget struct {
			regionName string
			orgGUID    string
			orgName    string
			apiURL     string
			token      string
			user       createXsuaaUser
		}
		var targets []createTarget
		for _, w := range clients {
			for _, u := range csvUsers {
				if !strings.EqualFold(u.OrgID, w.OrgGUID) {
					continue
				}
				if !strings.EqualFold(u.Region, w.RegionName) {
					continue
				}
				targets = append(targets, createTarget{
					regionName: w.RegionName,
					orgGUID:    w.OrgGUID,
					orgName:    w.OrgName,
					apiURL:     w.APIURL,
					token:      w.Token,
					user:       u,
				})
			}
		}

		if len(targets) == 0 {
			fmt.Fprintln(os.Stdout, "No matching orgs found.")
			return nil
		}

		// Apply --include / --skip filters.
		if includePattern != "" || skipPattern != "" {
			var filtered []createTarget
			for _, t := range targets {
				fields := []string{t.user.UserName, t.user.Email, strings.Join(t.user.Groups, ";")}
				if includePattern != "" && !skipMatches(includePattern, fields...) {
					continue
				}
				if skipPattern != "" && skipMatches(skipPattern, fields...) {
					continue
				}
				filtered = append(filtered, t)
			}
			targets = filtered
		}
		if len(targets) == 0 {
			fmt.Fprintln(os.Stdout, "No users to create after applying --include/--skip.")
			return nil
		}

		// Phase 3: assemble preview grouped by region then org.
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
			regionOrgs[t.regionName][t.orgGUID].Users = append(
				regionOrgs[t.regionName][t.orgGUID].Users,
				usrOutUser{
					Origin:   t.user.Origin,
					UserName: t.user.UserName,
					Email:    t.user.Email,
					Groups:   strings.Join(t.user.Groups, ";"),
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
			fmt.Fprintln(os.Stdout, "Users to be created:")
			os.Stdout.Write(out)
			fmt.Fprintln(os.Stdout)
			fmt.Fprint(os.Stdout, "Proceed with user creation? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Phase 4: create users and assign role collections.
		// Build a group displayName→ID map per XSUAA endpoint (once per org).
		fmt.Fprintln(os.Stdout, "Creating users...")
		groupIDs := make(map[string]map[string]string) // apiURL → displayName → groupID
		for _, t := range targets {
			if _, ok := groupIDs[t.apiURL]; ok {
				continue
			}
			ids, fetchErr := xsuaa.ListGroupIDs(ctx, t.apiURL, t.token)
			if fetchErr != nil {
				fmt.Fprintf(os.Stderr, "  ! [%s] %s: could not fetch group list: %v\n",
					t.regionName, t.orgName, fetchErr)
				groupIDs[t.apiURL] = map[string]string{} // empty — skip role assignments for this org
			} else {
				groupIDs[t.apiURL] = ids
			}
		}

		for _, t := range targets {
			u := t.user

			// Create user; nil return means HTTP 409 (already exists).
			created, createErr := xsuaa.CreateUser(ctx, t.apiURL, t.token, u.UserName, u.Origin, u.Email)
			if createErr != nil {
				fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: create failed: %v\n",
					t.regionName, t.orgName, u.UserName, createErr)
				continue
			}

			var userID string
			if created != nil {
				userID = created.ID
				fmt.Fprintf(os.Stdout, "  + [%s] %s / %s\n", t.regionName, t.orgName, u.UserName)
			} else {
				fmt.Fprintf(os.Stdout, "  ~ [%s] %s / %s (already exists)\n", t.regionName, t.orgName, u.UserName)
				// Look up the existing user's ID so we can add them to groups.
				var findErr error
				userID, findErr = xsuaa.FindUserID(ctx, t.apiURL, t.token, u.UserName, u.Origin)
				if findErr != nil {
					fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: could not find user ID: %v\n",
						t.regionName, t.orgName, u.UserName, findErr)
					continue
				}
			}

			orgGroups := groupIDs[t.apiURL]
			for _, rc := range u.Groups {
				groupID, found := orgGroups[rc]
				if !found {
					fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s / %q: role collection not found\n",
						t.regionName, t.orgName, u.UserName, rc)
					continue
				}
				if err := xsuaa.AddGroupMember(ctx, t.apiURL, t.token, groupID, userID, u.Origin); err != nil {
					fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s / %q: %v\n",
						t.regionName, t.orgName, u.UserName, rc, err)
				} else {
					fmt.Fprintf(os.Stdout, "    + role collection: %s\n", rc)
				}
			}
		}
		return nil
	},
}

func init() {
	createUsersCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(createUsersCmd)
	createUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	createUsersCmd.Flags().String("skip", "", "Skip users whose user_name, email, or groups contain this pattern (case-insensitive substring match)")
	createUsersCmd.Flags().String("include", "", "Only include users whose user_name, email, or groups contain this pattern (case-insensitive substring match)")
}
