package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// cosOrgRef is one row from the --orgs / --excludeOrgs CSV (region,id,name).
type cosOrgRef struct {
	Region string
	ID     string
	Name   string
}

// cosOrgSet is a list of org references used for include / exclude filtering.
type cosOrgSet []cosOrgRef

// matches reports whether an org (identified by region, GUID, and name) is
// covered by at least one entry in the set. A blank Region/ID/Name field in the
// reference is treated as a wildcard for that column.
func (s cosOrgSet) matches(region, orgGUID, orgName string) bool {
	for _, ref := range s {
		if ref.Region != "" && !strings.EqualFold(ref.Region, region) {
			continue
		}
		if ref.ID != "" && ref.ID == orgGUID {
			return true
		}
		if ref.Name != "" && strings.EqualFold(ref.Name, orgName) {
			return true
		}
		// Both ID and Name are blank → match any org in the region.
		if ref.ID == "" && ref.Name == "" {
			return true
		}
	}
	return false
}

func parseCosOrgCSV(path string) (cosOrgSet, error) {
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
	if len(header) < 3 || header[0] != "region" || header[1] != "org_id" || header[2] != "org_name" {
		return nil, fmt.Errorf("invalid header — expected: region,org_id,org_name")
	}

	var refs cosOrgSet
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
		refs = append(refs, cosOrgRef{
			Region: strings.TrimSpace(row[0]),
			ID:     strings.TrimSpace(row[1]),
			Name:   strings.TrimSpace(row[2]),
		})
	}
	return refs, nil
}

// ── preview document types ────────────────────────────────────────────────────

type cosPreviewUser struct {
	Name   string `toon:"cfuser_name"`
	Origin string `toon:"cfuser_origin"`
	Roles  string `toon:"cfuser_roles"`
}

type cosPreviewSpace struct {
	ID    string           `toon:"space_id"`
	Name  string           `toon:"space_name"`
	Users []cosPreviewUser `toon:"cfusers"`
}

type cosPreviewOrg struct {
	ID     string            `toon:"org_id"`
	Name   string            `toon:"org_name"`
	Users  []cosPreviewUser  `toon:"cfusers"` // org-level users
	Spaces []cosPreviewSpace `toon:"spaces"`
}

type cosPreviewRegion struct {
	ID   string          `toon:"region"`
	Orgs []cosPreviewOrg `toon:"orgs"`
}

type cosPreviewScope struct {
	Regions []cosPreviewRegion `toon:"regions"`
}

// buildCosPreviewDoc groups []orgSpaceUserRow into a cosPreviewScope by
// region → org → (org-level users, spaces with users), preserving input order.
func buildCosPreviewDoc(rows []orgSpaceUserRow) cosPreviewScope {
	var regionOrder []string
	regionIdx := map[string]int{}
	orgOrder := map[string][]string{}     // regionID → ordered orgIDs
	orgIdx := map[string]map[string]int{} // regionID → orgID → position
	spaceOrder := map[string][]string{}   // orgID → ordered spaceIDs
	spaceIdx := map[string]map[string]int{}
	orgInfo := map[string]cosPreviewOrg{}
	spaceInfo := map[string]cosPreviewSpace{}

	for _, row := range rows {
		if _, seen := regionIdx[row.Region]; !seen {
			regionIdx[row.Region] = len(regionOrder)
			regionOrder = append(regionOrder, row.Region)
			orgOrder[row.Region] = nil
			orgIdx[row.Region] = map[string]int{}
		}
		if _, seen := orgIdx[row.Region][row.OrgID]; !seen {
			orgIdx[row.Region][row.OrgID] = len(orgOrder[row.Region])
			orgOrder[row.Region] = append(orgOrder[row.Region], row.OrgID)
			orgInfo[row.OrgID] = cosPreviewOrg{ID: row.OrgID, Name: row.OrgName}
		}
		u := cosPreviewUser{
			Name:   row.UserName,
			Origin: row.Origin,
			Roles:  strings.Join(row.Roles, ";"),
		}
		if row.SpaceID == "" {
			// Org-level
			o := orgInfo[row.OrgID]
			o.Users = append(o.Users, u)
			orgInfo[row.OrgID] = o
		} else {
			// Space-level
			if spaceIdx[row.OrgID] == nil {
				spaceIdx[row.OrgID] = map[string]int{}
			}
			if _, seen := spaceIdx[row.OrgID][row.SpaceID]; !seen {
				spaceIdx[row.OrgID][row.SpaceID] = len(spaceOrder[row.OrgID])
				spaceOrder[row.OrgID] = append(spaceOrder[row.OrgID], row.SpaceID)
				spaceInfo[row.SpaceID] = cosPreviewSpace{ID: row.SpaceID, Name: row.SpaceName}
			}
			sp := spaceInfo[row.SpaceID]
			sp.Users = append(sp.Users, u)
			spaceInfo[row.SpaceID] = sp
		}
	}

	var previewRegions []cosPreviewRegion
	for _, regionID := range regionOrder {
		pr := cosPreviewRegion{ID: regionID}
		for _, orgID := range orgOrder[regionID] {
			po := orgInfo[orgID]
			for _, spaceID := range spaceOrder[orgID] {
				po.Spaces = append(po.Spaces, spaceInfo[spaceID])
			}
			pr.Orgs = append(pr.Orgs, po)
		}
		previewRegions = append(previewRegions, pr)
	}
	return cosPreviewScope{Regions: previewRegions}
}

// printCosPreview marshals doc as TOON and writes it to os.Stdout with header.
func printCosPreview(header string, doc cosPreviewScope) error {
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding preview: %w", err)
	}
	fmt.Fprintln(os.Stdout, header)
	os.Stdout.Write(out) //nolint:errcheck
	fmt.Fprintln(os.Stdout)
	return nil
}

// rowAPIURL resolves the CF API URL and token for a row's region field.
// It first tries treating region as a shorthand (e.g. "eu20") and deriving the
// standard API URL; if no token is found, it tries region as a direct URL.
// This dual-mode lookup is transparent to callers and makes test code work
// without any special-casing (httptest servers use their full URL as the region).
func rowAPIURL(region string, creds *store.Credentials) (string, store.RegionToken, bool) {
	apiURL := store.RegionToAPIURL(region)
	if tok, ok := creds.Tokens[apiURL]; ok {
		return apiURL, tok, true
	}
	if tok, ok := creds.Tokens[region]; ok {
		return region, tok, true
	}
	return "", store.RegionToken{}, false
}

// targetAPIURLs returns the CF API URLs to target for a given region value.
//
//   - If srcRegion is non-empty, it is treated as a region shorthand (e.g. "eu20")
//     or a direct URL. A single-element slice is returned if a token exists; nil
//     otherwise (with a warning printed to stderr).
//   - If srcRegion is empty (broadcast), all active API URLs from creds are
//     returned, filtered by regionsFilter when non-nil.
//
// The returned strings are full CF API base URLs (e.g. "https://api.cf.eu20.hana.ondemand.com").
func targetAPIURLs(srcRegion string, creds *store.Credentials, regionsFilter map[string]bool) []string {
	if srcRegion != "" {
		if regionsFilter != nil && !regionsFilter[srcRegion] {
			return nil
		}
		apiURL := store.RegionToAPIURL(srcRegion)
		if _, ok := creds.Tokens[apiURL]; ok {
			return []string{apiURL}
		}
		// Fallback: treat srcRegion as a direct URL (supports httptest servers in tests).
		if _, ok := creds.Tokens[srcRegion]; ok {
			return []string{srcRegion}
		}
		fmt.Fprintf(os.Stderr, "warning: no token for region %q — run: bo login --regions %s\n", srcRegion, srcRegion)
		return nil
	}
	// Broadcast: iterate all active API URLs matching regionsFilter.
	var urls []string
	for _, u := range creds.ActiveAPIURLs {
		region := store.APIURLToRegion(u)
		if regionsFilter != nil && !regionsFilter[region] {
			continue
		}
		if _, ok := creds.Tokens[u]; !ok {
			continue
		}
		urls = append(urls, u)
	}
	return urls
}

// ── command ───────────────────────────────────────────────────────────────────

var createOrgSpaceUsersCmd = &cobra.Command{
	Use:   "create-org-space-users <org-space-users.csv>",
	Short: "Add users with org and space roles from a CSV file",
	Long: `Add users to CF organizations and spaces from a CSV file.

Two CSV formats are accepted:

  Simple 3-column format (broadcast — no targeting info):
    cfuser_name,cfuser_origin,cfuser_roles

  Full 9-column format (produced by "bo org-space-users --format csv"):
    region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles

Broadcast semantics (empty fields in the CSV):
  - Empty region    → target all active CF regions stored in credentials.
  - Empty org_id    → discover and target all accessible CF orgs (filtered by
                       --orgs / --excludeOrgs).
  - Empty space_id  → if space_name is set, match spaces by name; if both are
                       empty and cfuser_roles contains space_* roles, target ALL
                       spaces in each resolved org.
  - Non-empty space_id  → assign space roles to that specific space only.

Roles are split by prefix: organization_* roles are applied at the org level;
space_* roles are applied at the space level.

Without -y, a TOON preview of all targeted users and scopes is shown and
confirmation is required before any changes are made.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile := args[0]
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		rows, err := parseOrgSpaceUsersCSV(usersFile)
		if err != nil {
			return fmt.Errorf("invalid users CSV: %w", err)
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
		// This may call CF APIs to discover orgs and spaces for broadcast rows.
		var activeRows []orgSpaceUserRow
		for _, srcRow := range rows {
			// Split roles by level.
			var orgRoles, spaceRoles []string
			for _, role := range srcRow.Roles {
				if strings.HasPrefix(role, "organization_") {
					orgRoles = append(orgRoles, role)
				} else if strings.HasPrefix(role, "space_") {
					spaceRoles = append(spaceRoles, role)
				}
			}
			if len(orgRoles) == 0 && len(spaceRoles) == 0 {
				slog.Debug("skipping row with no recognized roles", "user", srcRow.UserName)
				continue
			}

			apiURLs := targetAPIURLs(srcRow.Region, creds, regionsFilter)
			for _, apiURL := range apiURLs {
				tok := creds.Tokens[apiURL]
				client := getClient(apiURL, tok)
				region := store.APIURLToRegion(apiURL)

				// ── Case 1: specific space (SpaceID non-empty, spaceRoles present) ──
				// No org iteration needed — SpaceID is used directly.
				if len(spaceRoles) > 0 && srcRow.SpaceID != "" {
					// Apply org filter using srcRow info when available.
					if len(includeOrgs) > 0 && !includeOrgs.matches(region, srcRow.OrgID, srcRow.OrgName) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(region, srcRow.OrgID, srcRow.OrgName) {
						slog.Debug("skipping excluded org (space row)", "org", srcRow.OrgName)
						continue
					}
					activeRows = append(activeRows, orgSpaceUserRow{
						Region:    region,
						OrgID:     srcRow.OrgID,
						OrgName:   srcRow.OrgName,
						SpaceID:   srcRow.SpaceID,
						SpaceName: srcRow.SpaceName,
						UserName:  srcRow.UserName,
						Origin:    srcRow.Origin,
						Roles:     spaceRoles,
					})
				}

				// ── Case 2: org-level roles OR broadcast-space rows ──
				// Requires iterating over resolved orgs.
				if len(orgRoles) == 0 && !(len(spaceRoles) > 0 && srcRow.SpaceID == "") {
					continue
				}

				// Determine target orgs.
				var targetOrgs []cf.Organization
				if srcRow.OrgID != "" {
					targetOrgs = []cf.Organization{{GUID: srcRow.OrgID, Name: srcRow.OrgName}}
				} else {
					// Broadcast: discover all orgs.
					orgs, listErr := client.ListOrganizations(ctx)
					if listErr != nil {
						fmt.Fprintf(os.Stderr, "warning: could not list orgs for %s: %v\n", apiURL, listErr)
						continue
					}
					targetOrgs = orgs
				}

				for _, org := range targetOrgs {
					// Apply include/exclude org filter.
					if len(includeOrgs) > 0 && !includeOrgs.matches(region, org.GUID, org.Name) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(region, org.GUID, org.Name) {
						slog.Debug("skipping excluded org", "org", org.Name, "region", region)
						continue
					}

					// Org-level roles.
					if len(orgRoles) > 0 {
						activeRows = append(activeRows, orgSpaceUserRow{
							Region:   region,
							OrgID:    org.GUID,
							OrgName:  org.Name,
							SpaceID:  "",
							UserName: srcRow.UserName,
							Origin:   srcRow.Origin,
							Roles:    orgRoles,
						})
					}

					// Space-level roles with broadcast space targeting.
					if len(spaceRoles) > 0 && srcRow.SpaceID == "" {
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
								UserName:  srcRow.UserName,
								Origin:    srcRow.Origin,
								Roles:     spaceRoles,
							})
						}
					}
				}
			}
		}

		if len(activeRows) == 0 {
			return fmt.Errorf("no rows to process after filtering (check --regions, --orgs, or login)")
		}

		// Preview and confirm.
		if !skipConfirm {
			if err := printCosPreview("Users to be added:", buildCosPreviewDoc(activeRows)); err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, "Proceed with user creation? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Execute.
		for _, row := range activeRows {
			apiURL, tok, _ := rowAPIURL(row.Region, creds) // verified during expansion
			client := getClient(apiURL, tok)

			if row.SpaceID == "" {
				// Org-level roles.
				var added, failed []string
				for _, role := range row.Roles {
					if err := client.CreateOrganizationRole(ctx, role, row.UserName, row.Origin, row.OrgID); err != nil {
						failed = append(failed, role)
						slog.Debug("org role error", "user", row.UserName, "role", role, "org", row.OrgID, "err", err)
					} else {
						added = append(added, role)
					}
				}
				if len(added) > 0 {
					fmt.Fprintf(os.Stdout, "  + %s / %s (org) [%s]\n", row.UserName, row.OrgName, strings.Join(added, ", "))
				}
				for _, r := range failed {
					fmt.Fprintf(os.Stderr, "  ! %s / %s (org): failed role: %s\n", row.UserName, row.OrgName, r)
				}
			} else {
				// Space-level roles.
				var added, failed []string
				for _, role := range row.Roles {
					if err := client.CreateSpaceRole(ctx, role, row.UserName, row.Origin, row.SpaceID); err != nil {
						failed = append(failed, role)
						slog.Debug("space role error", "user", row.UserName, "role", role, "space", row.SpaceID, "err", err)
					} else {
						added = append(added, role)
					}
				}
				if len(added) > 0 {
					fmt.Fprintf(os.Stdout, "  + %s / %s / %s [%s]\n", row.UserName, row.OrgName, row.SpaceName, strings.Join(added, ", "))
				}
				for _, r := range failed {
					fmt.Fprintf(os.Stderr, "  ! %s / %s / %s: failed role: %s\n", row.UserName, row.OrgName, row.SpaceName, r)
				}
			}
		}
		return nil
	},
}

func init() {
	createOrgSpaceUsersCmd.GroupID = "cf-org"
	rootCmd.AddCommand(createOrgSpaceUsersCmd)
	createOrgSpaceUsersCmd.Flags().String("regions", "", "Only process rows whose region column matches one of these shorthands (e.g. us10,eu10); for broadcast rows (empty region), restricts which active regions are targeted")
	createOrgSpaceUsersCmd.Flags().String("orgs", "", "Path to orgs CSV file to include (columns: region,org_id,org_name); filters rows by org_id or org_name")
	createOrgSpaceUsersCmd.Flags().String("excludeOrgs", "", "Path to orgs CSV file to skip (columns: region,org_id,org_name)")
	createOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
