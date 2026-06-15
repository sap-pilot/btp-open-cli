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

// ── command ───────────────────────────────────────────────────────────────────

var createOrgSpaceUsersCmd = &cobra.Command{
	Use:   "create-org-space-users",
	Short: "Add users with org and space roles from an org-space-users CSV",
	Long: `Add users to CF organizations and spaces from a targeted CSV file.

The --users CSV must use the format produced by "bo org-space-users --format csv":

  region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles

Targeting rules:
  - Rows with an empty space_id → assign cfuser_roles to the org (org_id).
  - Rows with a non-empty space_id → assign cfuser_roles to that space (space_id).

Each row targets a specific org or space directly; no CF org/space discovery is
performed. The region column is used to select the correct CF API endpoint.

Use --orgs / --excludeOrgs to additionally filter which rows are applied based
on org_id / org_name. Use --regions to restrict processing to rows whose region
column matches one of the given shorthands (e.g. us10,eu20); when omitted all
rows are processed.

Without -y, a TOON preview of all targeted users and scopes is shown and
confirmation is required before any changes are made.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile, _ := cmd.Flags().GetString("users")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		rows, err := parseOrgSpaceUsersCSV(usersFile)
		if err != nil {
			return fmt.Errorf("invalid --users CSV: %w", err)
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

		// Filter rows: apply region, org include/exclude, and token availability.
		noTokenWarnedRegions := map[string]bool{}
		var activeRows []orgSpaceUserRow
		for _, row := range rows {
			if regionsFilter != nil && !regionsFilter[row.Region] {
				continue
			}
			if len(includeOrgs) > 0 && !includeOrgs.matches(row.Region, row.OrgID, row.OrgName) {
				continue
			}
			if len(excludeOrgs) > 0 && excludeOrgs.matches(row.Region, row.OrgID, row.OrgName) {
				slog.Debug("skipping excluded org row", "org", row.OrgName, "region", row.Region)
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
			return fmt.Errorf("no rows to process after filtering (check --regions, --orgs, or login)")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Preview and confirm.
		if !skipConfirm {
			if err := printCosPreview("Users to be added:", buildCosPreviewDoc(activeRows)); err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Proceed with user creation? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
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

		// Execute.
		for _, row := range activeRows {
			apiURL, tok, _ := rowAPIURL(row.Region, creds) // already verified above
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
	createOrgSpaceUsersCmd.Flags().String("regions", "", "Only process rows whose region column matches one of these shorthands (e.g. us10,eu10)")
	createOrgSpaceUsersCmd.Flags().String("users", "", "Path to org-space-users CSV file (required; columns: region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles)")
	createOrgSpaceUsersCmd.MarkFlagRequired("users")
	createOrgSpaceUsersCmd.Flags().String("orgs", "", "Path to orgs CSV file to include (columns: region,org_id,org_name); filters rows by org_id or org_name")
	createOrgSpaceUsersCmd.Flags().String("excludeOrgs", "", "Path to orgs CSV file to skip (columns: region,org_id,org_name)")
	createOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
