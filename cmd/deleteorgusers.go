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
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// parseDeleteUsersCSV parses a CSV with header "name,origin".
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
	if len(header) < 2 || header[0] != "cfuser_name" || header[1] != "cfuser_origin" {
		return nil, fmt.Errorf("invalid header — expected: cfuser_name,cfuser_origin")
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

// ── discovery plan types ──────────────────────────────────────────────────────

// delUserEntry pairs a CF user record with the role GUIDs to be removed.
type delUserEntry struct {
	User  cf.CfUser
	Roles []cf.Role
}

type delSpacePlan struct {
	Space cf.Space
	Users []delUserEntry
}

type delOrgPlan struct {
	Org    cf.Organization
	Users  []delUserEntry // org-level roles to remove
	Spaces []delSpacePlan
}

type delRegionPlan struct {
	Region string
	APIURL string
	Orgs   []delOrgPlan
}

// ── preview TOON types ────────────────────────────────────────────────────────

type delPreviewUser struct {
	ID     string `toon:"cfuser_id"`
	Name   string `toon:"cfuser_name"`
	Origin string `toon:"cfuser_origin"`
	Roles  string `toon:"cfuser_roles"`
}

type delPreviewSpace struct {
	ID    string           `toon:"space_id"`
	Name  string           `toon:"space_name"`
	Users []delPreviewUser `toon:"cfusers"`
}

type delPreviewOrg struct {
	ID     string            `toon:"org_id"`
	Name   string            `toon:"org_name"`
	Users  []delPreviewUser  `toon:"cfusers"`
	Spaces []delPreviewSpace `toon:"spaces"`
}

type delPreviewRegion struct {
	ID   string          `toon:"region"`
	Orgs []delPreviewOrg `toon:"orgs"`
}

type delPreviewDoc struct {
	Regions []delPreviewRegion `toon:"regions"`
}

// ── command ───────────────────────────────────────────────────────────────────

var deleteOrgSpaceUsersCmd = &cobra.Command{
	Use:   "delete-org-space-users",
	Short: "Remove users from all spaces and organizations across accessible CF orgs",
	Long: `Remove users from every space and organization across one or more regions.

The --users CSV must have the header: cfuser_name,cfuser_origin

Space-level role assignments are deleted first; after a 5-second pause (to allow
CF's async role processing to settle) org-level roles are then removed.

Without -y, a TOON preview of all roles to be deleted is shown before execution,
and confirmation is required.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		usersFile, _ := cmd.Flags().GetString("users")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		csvUsers, err := parseDeleteUsersCSV(usersFile)
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

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Phase 1: discover current roles for every target user in every region/org/space.
		plans := make([]delRegionPlan, len(apiURLs))
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
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
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))
				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, err)
					mu.Unlock()
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				var orgPlans []delOrgPlan
				for _, org := range orgs {
					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: error listing spaces: %v\n", regionName, org.Name, err)
						mu.Unlock()
					}

					spacePlans := make([]delSpacePlan, len(spaces))
					for si, sp := range spaces {
						spacePlans[si] = delSpacePlan{Space: sp}
					}

					var orgUserEntries []delUserEntry

					for _, u := range csvUsers {
						cfUser, err := client.FindCfUser(ctx, u.Name, u.Origin)
						if err != nil {
							slog.Debug("user not found", "user", u.Name, "region", regionName, "org", org.Name)
							continue
						}

						// Collect org-level roles.
						orgRoles, err := client.ListOrganizationUserRoles(ctx, org.GUID, cfUser.GUID)
						if err != nil {
							mu.Lock()
							fmt.Fprintf(os.Stderr, "[%s] %s / %s: could not list org roles: %v\n", regionName, org.Name, u.Name, err)
							mu.Unlock()
						} else if len(orgRoles) > 0 {
							orgUserEntries = append(orgUserEntries, delUserEntry{User: *cfUser, Roles: orgRoles})
						}

						// Collect space-level roles per space.
						for si, sp := range spaces {
							spaceRoles, err := client.ListSpaceUserRoles(ctx, sp.GUID, cfUser.GUID)
							if err != nil {
								mu.Lock()
								fmt.Fprintf(os.Stderr, "[%s] %s / %s / %s: could not list space roles: %v\n", regionName, org.Name, sp.Name, u.Name, err)
								mu.Unlock()
								continue
							}
							if len(spaceRoles) > 0 {
								spacePlans[si].Users = append(spacePlans[si].Users, delUserEntry{User: *cfUser, Roles: spaceRoles})
							}
						}
					}

					// Prune spaces with nothing to delete.
					var activeSpaces []delSpacePlan
					for _, sp := range spacePlans {
						if len(sp.Users) > 0 {
							activeSpaces = append(activeSpaces, sp)
						}
					}

					if len(orgUserEntries) > 0 || len(activeSpaces) > 0 {
						orgPlans = append(orgPlans, delOrgPlan{
							Org:    org,
							Users:  orgUserEntries,
							Spaces: activeSpaces,
						})
					}
				}
				plans[idx] = delRegionPlan{Region: regionName, APIURL: url, Orgs: orgPlans}
			}(i, apiURL)
		}
		wg.Wait()

		// Phase 2: preview and confirmation.
		if !skipConfirm {
			if err := delPrintPreview(plans); err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Proceed with cfuser deletion? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Phase 3a: delete all space-level roles.
		fmt.Fprintln(os.Stdout, "Deleting space cfusers...")
		for _, plan := range plans {
			if plan.APIURL == "" {
				continue
			}
			tok, ok := creds.Tokens[plan.APIURL]
			if !ok {
				continue
			}
			client := cf.NewClient(plan.APIURL, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(plan.APIURL, tok.AccessToken))

			for _, op := range plan.Orgs {
				for _, sp := range op.Spaces {
					for _, ue := range sp.Users {
						var removed, failed []string
						for _, role := range ue.Roles {
							if err := client.DeleteRole(ctx, role.GUID); err != nil {
								failed = append(failed, fmt.Sprintf("%s (%v)", role.Type, err))
							} else {
								removed = append(removed, role.Type)
							}
						}
						if len(removed) > 0 {
							fmt.Fprintf(os.Stdout, "  - %s / %s / %s [%s]\n",
								ue.User.Username, op.Org.Name, sp.Space.Name, strings.Join(removed, ", "))
						}
						for _, e := range failed {
							fmt.Fprintf(os.Stderr, "  ! %s / %s / %s: failed: %s\n",
								ue.User.Username, op.Org.Name, sp.Space.Name, e)
						}
					}
				}
			}
		}

		// Wait 5 seconds for CF's async role-deletion processing to complete.
		fmt.Fprintln(os.Stdout, "\nWaiting 5 s for CF async processing...")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}

		// Phase 3b: delete all org-level roles.
		fmt.Fprintln(os.Stdout, "Deleting org cfusers...")
		for _, plan := range plans {
			if plan.APIURL == "" {
				continue
			}
			tok, ok := creds.Tokens[plan.APIURL]
			if !ok {
				continue
			}
			client := cf.NewClient(plan.APIURL, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(plan.APIURL, tok.AccessToken))

			for _, op := range plan.Orgs {
				for _, ue := range op.Users {
					var removed, failed []string
					for _, role := range ue.Roles {
						if err := client.DeleteRole(ctx, role.GUID); err != nil {
							failed = append(failed, fmt.Sprintf("%s (%v)", role.Type, err))
						} else {
							removed = append(removed, role.Type)
						}
					}
					if len(removed) > 0 {
						fmt.Fprintf(os.Stdout, "  - %s / %s [%s]\n",
							ue.User.Username, op.Org.Name, strings.Join(removed, ", "))
					}
					for _, e := range failed {
						fmt.Fprintf(os.Stderr, "  ! %s / %s: failed: %s\n",
							ue.User.Username, op.Org.Name, e)
					}
				}
			}
		}
		return nil
	},
}

// delPrintPreview writes a TOON document of all roles that will be deleted.
func delPrintPreview(plans []delRegionPlan) error {
	var previewRegions []delPreviewRegion
	for _, plan := range plans {
		if len(plan.Orgs) == 0 {
			continue
		}
		pr := delPreviewRegion{ID: plan.Region}
		for _, op := range plan.Orgs {
			po := delPreviewOrg{ID: op.Org.GUID, Name: op.Org.Name}
			for _, ue := range op.Users {
				po.Users = append(po.Users, delPreviewUser{
					ID:     ue.User.GUID,
					Name:   ue.User.Username,
					Origin: ue.User.Origin,
					Roles:  joinRoleTypes(ue.Roles),
				})
			}
			for _, sp := range op.Spaces {
				ps := delPreviewSpace{ID: sp.Space.GUID, Name: sp.Space.Name}
				for _, ue := range sp.Users {
					ps.Users = append(ps.Users, delPreviewUser{
						ID:     ue.User.GUID,
						Name:   ue.User.Username,
						Origin: ue.User.Origin,
						Roles:  joinRoleTypes(ue.Roles),
					})
				}
				po.Spaces = append(po.Spaces, ps)
			}
			pr.Orgs = append(pr.Orgs, po)
		}
		previewRegions = append(previewRegions, pr)
	}

	out, err := toonenc.Marshal(delPreviewDoc{Regions: previewRegions}, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding preview: %w", err)
	}
	fmt.Fprintln(os.Stdout, "cfusers to be deleted:")
	os.Stdout.Write(out)
	fmt.Fprintln(os.Stdout)
	return nil
}

func joinRoleTypes(roles []cf.Role) string {
	types := make([]string, len(roles))
	for i, r := range roles {
		types[i] = r.Type
	}
	return strings.Join(types, ";")
}

func init() {
	rootCmd.AddCommand(deleteOrgSpaceUsersCmd)
	deleteOrgSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteOrgSpaceUsersCmd.Flags().String("users", "", "Path to the CSV file (required; columns: cfuser_name,cfuser_origin)")
	deleteOrgSpaceUsersCmd.MarkFlagRequired("users")
	deleteOrgSpaceUsersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
