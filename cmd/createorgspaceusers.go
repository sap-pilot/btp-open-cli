package cmd

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

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
	if len(header) < 3 || header[0] != "region" || header[1] != "id" || header[2] != "name" {
		return nil, fmt.Errorf("invalid header — expected: region,id,name")
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

// ── preview document types (printed before user confirmation) ─────────────────

type cosPreviewSpace struct {
	ID   string `toon:"id"`
	Name string `toon:"name"`
}

type cosPreviewOrg struct {
	ID     string            `toon:"id"`
	Name   string            `toon:"name"`
	Spaces []cosPreviewSpace `toon:"spaces"`
}

type cosPreviewRegion struct {
	ID   string          `toon:"id"`
	Orgs []cosPreviewOrg `toon:"orgs"`
}

type cosPreviewScope struct {
	Regions []cosPreviewRegion `toon:"regions"`
}

type cosPreviewUser struct {
	Name   string `toon:"name"`
	Origin string `toon:"origin"`
	Roles  string `toon:"roles"`
}

type cosPreviewUsers struct {
	Users []cosPreviewUser `toon:"users"`
}

// ── execution plan types ──────────────────────────────────────────────────────

type cosOrgPlan struct {
	Org    cf.Organization
	Spaces []cf.Space
}

type cosRegionPlan struct {
	Region string
	APIURL string
	Orgs   []cosOrgPlan
}

// ── command ───────────────────────────────────────────────────────────────────

var createOrgSpaceUsersCmd = &cobra.Command{
	Use:   "create-org-space-users",
	Short: "Add users with org and space roles across accessible CF orgs",
	Long: `Add users from a CSV file to CF organizations and their spaces across one or more regions.

The --users CSV must have the header: name,origin,roles
Roles are semicolon-separated and may mix org-level and space-level roles:
  organization_user;organization_manager;space_developer;space_manager

Org-level roles (organization_*) are assigned to each target org.
Space-level roles (space_*) are assigned to every space within each target org.

Use --orgs to restrict to specific orgs (CSV: region,id,name).
Use --excludeOrgs to skip orgs such as production environments (same CSV format).

Without -y, a TOON preview of the target orgs/spaces and users is shown, and
confirmation is required before any changes are made.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile, _ := cmd.Flags().GetString("users")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		// 1. Parse the users CSV.
		users, err := parseUsersCSV(usersFile)
		if err != nil {
			return fmt.Errorf("invalid --users CSV: %w", err)
		}

		// 2. Parse optional org filter CSVs.
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

		// 3. Resolve API URLs from --regions flag or stored login.
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

		// 4. Fetch orgs + spaces per region in parallel, preserving input order.
		plans := make([]cosRegionPlan, len(apiURLs))
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				slog.Debug("fetching region for plan", "region", regionName)

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

				var orgPlans []cosOrgPlan
				for _, org := range orgs {
					if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
						slog.Debug("skipping excluded org", "org", org.Name, "region", regionName)
						continue
					}

					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: error listing spaces: %v\n", regionName, org.Name, err)
						mu.Unlock()
					}
					orgPlans = append(orgPlans, cosOrgPlan{Org: org, Spaces: spaces})
				}
				plans[idx] = cosRegionPlan{Region: regionName, APIURL: url, Orgs: orgPlans}
			}(i, apiURL)
		}
		wg.Wait()

		// 5. Show TOON preview and prompt for confirmation unless -y was given.
		if !skipConfirm {
			if err := cosPrintPreview(plans, users); err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Proceed with user creation? [y/N] ")
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// 6. Execute: assign roles per region → org → user.
		for _, plan := range plans {
			if plan.APIURL == "" {
				continue
			}
			tok, ok := creds.Tokens[plan.APIURL]
			if !ok {
				continue
			}
			client := cf.NewClient(plan.APIURL, tok.AccessToken)

			for _, op := range plan.Orgs {
				fmt.Fprintf(os.Stdout, "[%s] %s:\n", plan.Region, op.Org.Name)

				for _, u := range users {
					var orgRoles, spaceRoles []string
					for _, role := range u.Roles {
						switch {
						case strings.HasPrefix(role, "organization_"):
							orgRoles = append(orgRoles, role)
						case strings.HasPrefix(role, "space_"):
							spaceRoles = append(spaceRoles, role)
						default:
							fmt.Fprintf(os.Stderr, "  ~ %s: unrecognised role %q — skipping\n", u.Name, role)
						}
					}

					// Org-level roles.
					var addedOrg, failedOrg []string
					for _, role := range orgRoles {
						if err := client.CreateOrganizationRole(ctx, role, u.Name, u.Origin, op.Org.GUID); err != nil {
							failedOrg = append(failedOrg, role)
							slog.Debug("org role error", "user", u.Name, "role", role, "err", err)
						} else {
							addedOrg = append(addedOrg, role)
						}
					}
					if len(addedOrg) > 0 {
						fmt.Fprintf(os.Stdout, "  + %s (org) [%s]\n", u.Name, strings.Join(addedOrg, ", "))
					}
					for _, r := range failedOrg {
						fmt.Fprintf(os.Stderr, "  ! %s (org): failed role: %s\n", u.Name, r)
					}

					// Space-level roles — applied to every space in the org.
					for _, space := range op.Spaces {
						var addedSp, failedSp []string
						for _, role := range spaceRoles {
							if err := client.CreateSpaceRole(ctx, role, u.Name, u.Origin, space.GUID); err != nil {
								failedSp = append(failedSp, role)
								slog.Debug("space role error", "user", u.Name, "role", role, "space", space.Name, "err", err)
							} else {
								addedSp = append(addedSp, role)
							}
						}
						if len(addedSp) > 0 {
							fmt.Fprintf(os.Stdout, "  + %s / %s [%s]\n", u.Name, space.Name, strings.Join(addedSp, ", "))
						}
						for _, r := range failedSp {
							fmt.Fprintf(os.Stderr, "  ! %s / %s: failed role: %s\n", u.Name, space.Name, r)
						}
					}
				}
			}
		}
		return nil
	},
}

// cosPrintPreview writes TOON previews of the target scope and the users list.
func cosPrintPreview(plans []cosRegionPlan, users []csvUser) error {
	var previewRegions []cosPreviewRegion
	for _, plan := range plans {
		if len(plan.Orgs) == 0 {
			continue
		}
		pr := cosPreviewRegion{ID: plan.Region}
		for _, op := range plan.Orgs {
			po := cosPreviewOrg{ID: op.Org.GUID, Name: op.Org.Name}
			for _, sp := range op.Spaces {
				po.Spaces = append(po.Spaces, cosPreviewSpace{ID: sp.GUID, Name: sp.Name})
			}
			pr.Orgs = append(pr.Orgs, po)
		}
		previewRegions = append(previewRegions, pr)
	}

	scopeOut, err := toonenc.Marshal(cosPreviewScope{Regions: previewRegions}, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding scope preview: %w", err)
	}

	var previewUsers []cosPreviewUser
	for _, u := range users {
		previewUsers = append(previewUsers, cosPreviewUser{
			Name:   u.Name,
			Origin: u.Origin,
			Roles:  strings.Join(u.Roles, ";"),
		})
	}
	usersOut, err := toonenc.Marshal(cosPreviewUsers{Users: previewUsers}, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding users preview: %w", err)
	}

	fmt.Fprintln(os.Stdout, "Target organizations and spaces:")
	os.Stdout.Write(scopeOut)
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Users to be added:")
	os.Stdout.Write(usersOut)
	fmt.Fprintln(os.Stdout)
	return nil
}

func init() {
	rootCmd.AddCommand(createOrgSpaceUsersCmd)
	createOrgSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	createOrgSpaceUsersCmd.Flags().String("users", "", "Path to users CSV file (required; columns: name,origin,roles)")
	createOrgSpaceUsersCmd.MarkFlagRequired("users")
	createOrgSpaceUsersCmd.Flags().String("orgs", "", "Path to orgs CSV file to target (columns: region,id,name); targets all orgs if omitted")
	createOrgSpaceUsersCmd.Flags().String("excludeOrgs", "", "Path to orgs CSV file to skip (columns: region,id,name)")
	createOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
