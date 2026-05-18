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

type orgDetail struct {
	Org   cf.Organization
	Users []cf.CfUser
	Roles map[string][]string // userGUID → role types
}

type regionData struct {
	Region string
	Orgs   []orgDetail
	Err    error
}

// ── shared output document model (JSON + TOON tags) ─────────────────────────

type outUser struct {
	ID     string `json:"id"     toon:"id"`
	Name   string `json:"name"   toon:"name"`
	Origin string `json:"origin" toon:"origin"`
	Roles  string `json:"roles"  toon:"roles"`
}

type outOrg struct {
	ID    string    `json:"id"    toon:"id"`
	Name  string    `json:"name"  toon:"name"`
	Users []outUser `json:"users" toon:"users"`
}

type outRegion struct {
	ID   string   `json:"id"   toon:"id"`
	Orgs []outOrg `json:"orgs" toon:"orgs"`
}

type outDoc struct {
	Regions []outRegion `json:"regions" toon:"regions"`
}

// userMatchesFilter reports whether any of a user's id, name, or origin
// contains the filter string (case-insensitive). Always true when filter is "".
func userMatchesFilter(u outUser, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(u.ID), f) ||
		strings.Contains(strings.ToLower(u.Name), f) ||
		strings.Contains(strings.ToLower(u.Origin), f) ||
		strings.Contains(strings.ToLower(u.Roles), f)
}

// buildOutputDoc converts raw fetch results into the shared output model.
// filter is an optional substring applied to user id/name/origin; orgs and
// regions with no matching users are omitted from the result.
func buildOutputDoc(results []regionData, filter string) (outDoc, []error) {
	var doc outDoc
	var errs []error
	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", r.Region, r.Err))
			continue
		}
		or := outRegion{ID: r.Region}
		for _, od := range r.Orgs {
			oo := outOrg{ID: od.Org.GUID, Name: od.Org.Name}
			for _, u := range od.Users {
				ou := outUser{
					ID:     u.GUID,
					Name:   u.Username,
					Origin: u.Origin,
					Roles:  strings.Join(od.Roles[u.GUID], ";"),
				}
				if userMatchesFilter(ou, filter) {
					oo.Users = append(oo.Users, ou)
				}
			}
			if len(oo.Users) > 0 {
				or.Orgs = append(or.Orgs, oo)
			}
		}
		if len(or.Orgs) > 0 {
			doc.Regions = append(doc.Regions, or)
		}
	}
	return doc, errs
}

// ── command ─────────────────────────────────────────────────────────────────

var orgUsersCmd = &cobra.Command{
	Use:   "org-users",
	Short: "List all org users across every accessible organization",
	Long: `List users in every CF organization across one or more regions.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   CSV rows: region,org_id,org_name,user_id,user_name,user_origin

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		filter, _ := cmd.Flags().GetString("filter")

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		// Determine API URLs: --regions flag > stored ActiveAPIURLs.
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

		// Fetch each region's data in parallel, preserving input order.
		results := make([]regionData, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				slog.Debug("fetching region", "region", regionName)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = regionData{
						Region: regionName,
						Err:    fmt.Errorf("no token — run: bo login --regions %s", regionName),
					}
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = regionData{Region: regionName, Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				allRoles, err := client.ListAllRoles(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not fetch roles for %s: %v\n", regionName, err)
					allRoles = cf.AllRoles{OrgRoles: map[string]map[string][]string{}}
				}
				slog.Debug("roles fetched", "region", regionName)

				details := make([]orgDetail, 0, len(orgs))
				for _, org := range orgs {
					users, err := client.ListOrganizationUsers(ctx, org.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: skipping org %q in %s: %v\n", org.Name, regionName, err)
						continue
					}
					roles := allRoles.OrgRoles[org.GUID]
					if roles == nil {
						roles = map[string][]string{}
					}
					details = append(details, orgDetail{Org: org, Users: users, Roles: roles})
				}
				results[idx] = regionData{Region: regionName, Orgs: details}
			}(i, apiURL)
		}
		wg.Wait()

		switch strings.ToLower(format) {
		case "json":
			return writeOrgUsersJSON(results, filter)
		case "csv":
			return writeOrgUsersCSV(results, filter)
		default: // "toon"
			return writeOrgUsersToon(results, filter)
		}
	},
}

// writeOrgUsersToon serializes the output document via the TOON encoder.
// The library automatically produces a compact tabular representation for the
// uniform users slice, e.g.:
//
//	regions[1]:
//	  - id: us10
//	    orgs[1]:
//	      - id: abc-123
//	        name: my-org
//	        users[2]{id,name,origin}:
//	          xyz-789,user@example.com,sap.ids
//	          xyz-111,admin@example.com,uaa
func writeOrgUsersToon(results []regionData, filter string) error {
	doc, errs := buildOutputDoc(results, filter)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err = os.Stdout.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout)
	return err
}

// writeOrgUsersJSON serializes the output document as indented JSON.
func writeOrgUsersJSON(results []regionData, filter string) error {
	doc, errs := buildOutputDoc(results, filter)
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

// writeOrgUsersCSV writes region,org_id,org_name,user_id,user_name,user_origin rows.
func writeOrgUsersCSV(results []regionData, filter string) error {
	doc, errs := buildOutputDoc(results, filter)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	if err := w.Write([]string{"region", "org_id", "org_name", "user_id", "user_name", "user_origin", "user_roles"}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				if err := w.Write([]string{
					r.ID, o.ID, o.Name, u.ID, u.Name, u.Origin, u.Roles,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(orgUsersCmd)
	orgUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgUsersCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
	orgUsersCmd.Flags().String("filter", "", "Case-insensitive substring filter applied to user id, name, and origin")
}
