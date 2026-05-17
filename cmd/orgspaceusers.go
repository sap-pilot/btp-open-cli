package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
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

// ── internal fetch types ────────────────────────────────────────────────────

type ospSpaceDetail struct {
	Space cf.Space
	Users []cf.User
	Roles map[string][]string // userGUID → role types
}

type ospOrgDetail struct {
	Org    cf.Organization
	Users  []cf.User
	Roles  map[string][]string // userGUID → role types
	Spaces []ospSpaceDetail
}

type ospRegionData struct {
	Region string
	Orgs   []ospOrgDetail
	Err    error
}

// ── output document model ────────────────────────────────────────────────────

type ospOutSpace struct {
	ID    string    `json:"id"    toon:"id"`
	Name  string    `json:"name"  toon:"name"`
	Users []outUser `json:"users" toon:"users"`
}

type ospOutOrg struct {
	ID     string        `json:"id"     toon:"id"`
	Name   string        `json:"name"   toon:"name"`
	Users  []outUser     `json:"users"  toon:"users"`
	Spaces []ospOutSpace `json:"spaces" toon:"spaces"`
}

type ospOutRegion struct {
	ID   string      `json:"id"   toon:"id"`
	Orgs []ospOutOrg `json:"orgs" toon:"orgs"`
}

type ospOutDoc struct {
	Regions []ospOutRegion `json:"regions" toon:"regions"`
}

func buildOspOutputDoc(results []ospRegionData) (ospOutDoc, []error) {
	var doc ospOutDoc
	var errs []error
	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", r.Region, r.Err))
			continue
		}
		or := ospOutRegion{ID: r.Region}
		for _, od := range r.Orgs {
			oo := ospOutOrg{ID: od.Org.GUID, Name: od.Org.Name}
			for _, u := range od.Users {
				oo.Users = append(oo.Users, outUser{
					ID:     u.GUID,
					Name:   u.Username,
					Origin: u.Origin,
					Roles:  strings.Join(od.Roles[u.GUID], ";"),
				})
			}
			for _, sd := range od.Spaces {
				sp := ospOutSpace{ID: sd.Space.GUID, Name: sd.Space.Name}
				for _, u := range sd.Users {
					sp.Users = append(sp.Users, outUser{
						ID:     u.GUID,
						Name:   u.Username,
						Origin: u.Origin,
						Roles:  strings.Join(sd.Roles[u.GUID], ";"),
					})
				}
				oo.Spaces = append(oo.Spaces, sp)
			}
			or.Orgs = append(or.Orgs, oo)
		}
		doc.Regions = append(doc.Regions, or)
	}
	return doc, errs
}

// ── command ──────────────────────────────────────────────────────────────────

var orgSpaceUsersCmd = &cobra.Command{
	Use:   "org-space-users",
	Short: "List org and space users across every accessible organization",
	Long: `List users at the org and space level across one or more regions.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   CSV rows: region,org_id,org_name,scope,scope_id,scope_name,user_id,user_name,user_origin,user_roles

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")

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

		results := make([]ospRegionData, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				slog.Debug("fetching region", "region", regionName)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = ospRegionData{
						Region: regionName,
						Err:    fmt.Errorf("no token — run: bo login --regions %s", regionName),
					}
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = ospRegionData{Region: regionName, Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				var orgDetails []ospOrgDetail
				for _, org := range orgs {
					users, err := client.ListOrganizationUsers(ctx, org.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: skipping org %q in %s: %v\n", org.Name, regionName, err)
						continue
					}

					roles, err := client.ListOrganizationRoles(ctx, org.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not fetch org roles for %q in %s: %v\n", org.Name, regionName, err)
						roles = map[string][]string{}
					}

					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not fetch spaces for org %q in %s: %v\n", org.Name, regionName, err)
						spaces = nil
					}

					var spaceDetails []ospSpaceDetail
					for _, space := range spaces {
						slog.Debug("fetching space", "space", space.Name, "org", org.Name, "region", regionName)
						spaceUsers, err := client.ListSpaceUsers(ctx, space.GUID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "warning: skipping space %q in org %q: %v\n", space.Name, org.Name, err)
							continue
						}
						spaceRoles, err := client.ListSpaceRoles(ctx, space.GUID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "warning: could not fetch space roles for %q in org %q: %v\n", space.Name, org.Name, err)
							spaceRoles = map[string][]string{}
						}
						spaceDetails = append(spaceDetails, ospSpaceDetail{
							Space: space,
							Users: spaceUsers,
							Roles: spaceRoles,
						})
					}

					orgDetails = append(orgDetails, ospOrgDetail{
						Org:    org,
						Users:  users,
						Roles:  roles,
						Spaces: spaceDetails,
					})
				}
				results[idx] = ospRegionData{Region: regionName, Orgs: orgDetails}
			}(i, apiURL)
		}
		wg.Wait()

		switch strings.ToLower(format) {
		case "json":
			return writeOspJSON(results)
		case "csv":
			return writeOspCSV(results)
		default: // "toon"
			return writeOspToon(results)
		}
	},
}

func writeOspToon(results []ospRegionData) error {
	doc, errs := buildOspOutputDoc(results)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	_, err = os.Stdout.Write(out)
	return err
}

func writeOspJSON(results []ospRegionData) error {
	doc, errs := buildOspOutputDoc(results)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	return nil
}

// writeOspCSV writes one row per user. The "scope" column is "org" for
// org-level users and "space" for space-level users; scope_id/scope_name
// identify the org or space respectively.
func writeOspCSV(results []ospRegionData) error {
	doc, errs := buildOspOutputDoc(results)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	if err := w.Write([]string{
		"region", "org_id", "org_name",
		"scope", "scope_id", "scope_name",
		"user_id", "user_name", "user_origin", "user_roles",
	}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				if err := w.Write([]string{
					r.ID, o.ID, o.Name,
					"org", o.ID, o.Name,
					u.ID, u.Name, u.Origin, u.Roles,
				}); err != nil {
					return err
				}
			}
			for _, sp := range o.Spaces {
				for _, u := range sp.Users {
					if err := w.Write([]string{
						r.ID, o.ID, o.Name,
						"space", sp.ID, sp.Name,
						u.ID, u.Name, u.Origin, u.Roles,
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(orgSpaceUsersCmd)
	orgSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgSpaceUsersCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
}
