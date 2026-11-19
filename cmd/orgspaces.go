package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ─────────────────────────────────────────────────────────────

type osOutSpace struct {
	ID   string `json:"space_id"   toon:"space_id"`
	Name string `json:"space_name" toon:"space_name"`
}

type osOutOrg struct {
	ID     string       `json:"org_id"   toon:"org_id"`
	Name   string       `json:"org_name" toon:"org_name"`
	Spaces []osOutSpace `json:"spaces"   toon:"spaces"`
}

type osOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []osOutOrg `json:"orgs"   toon:"orgs"`
}

type osOutDoc struct {
	Regions []osOutRegion `json:"regions" toon:"regions"`
}

// osFlatRow is the flat, one-row-per-space view of osOutDoc — the same
// shape as the csv output. Used directly for csv, and for json/toon too
// when --fields or --sort-by is given; otherwise json/toon keep the nested
// region/org structure.
type osFlatRow struct {
	Region    string `json:"region"     toon:"region"`
	OrgID     string `json:"org_id"     toon:"org_id"`
	OrgName   string `json:"org_name"   toon:"org_name"`
	SpaceID   string `json:"space_id"   toon:"space_id"`
	SpaceName string `json:"space_name" toon:"space_name"`
}

// flattenOrgSpacesRows converts doc's nested region/org structure into one
// flat row per space.
func flattenOrgSpacesRows(doc osOutDoc) []osFlatRow {
	var rows []osFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				rows = append(rows, osFlatRow{Region: r.ID, OrgID: o.ID, OrgName: o.Name, SpaceID: sp.ID, SpaceName: sp.Name})
			}
		}
	}
	return rows
}

// ── command ───────────────────────────────────────────────────────────────────

var orgSpacesCmd = &cobra.Command{
	Use:   "org-spaces",
	Short: "List all accessible CF organizations and their spaces",
	Long: `List all Cloud Foundry organizations and spaces the authenticated user can access.

Output format (TOON by default):
  regions:
    - region: us10
      orgs:
        - org_id: <guid>
          org_name: my-org
          spaces:
            - space_id: <guid>
              space_name: dev

Use --format json for JSON output, or --format csv for a flat CSV with columns:
  region,org_id,org_name,space_id,space_name

json/toon normally nest regions/orgs/spaces instead of that flat shape, but
switch to that same flat {"rows": [...]} view whenever --fields or
--sort-by is given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
csv columns above) to limit output to just those columns, in the order
given. Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep the default order:
sorted by space_name within each org.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs, err := parseFieldsFlag[osFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[osFlatRow](sortByFlag, "--sort-by")
		if err != nil {
			return err
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

		// Fetch orgs and all spaces in parallel per region.
		type regionResult struct {
			region string
			orgs   []cf.Organization
			spaces map[string][]cf.Space // orgGUID → spaces
			err    error
		}
		results := make([]regionResult, len(apiURLs))
		var wg sync.WaitGroup

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = regionResult{region: regionName,
						err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = regionResult{region: regionName, err: err}
					return
				}

				spaces, err := client.ListAllSpaces(ctx)
				if err != nil {
					results[idx] = regionResult{region: regionName, err: err}
					return
				}
				results[idx] = regionResult{region: regionName, orgs: orgs, spaces: spaces}
			}(i, apiURL)
		}
		wg.Wait()

		// Build output document preserving region order.
		var outRegions []osOutRegion
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] error: %v\n", r.region, r.err)
				continue
			}
			var outOrgs []osOutOrg
			for _, org := range r.orgs {
				spaces := r.spaces[org.GUID]
				sort.Slice(spaces, func(i, j int) bool {
					return spaces[i].Name < spaces[j].Name
				})
				var outSpaces []osOutSpace
				for _, sp := range spaces {
					if includePattern != "" && !skipMatches(includePattern, org.Name, sp.Name) {
						continue
					}
					if excludePattern != "" && skipMatches(excludePattern, org.Name, sp.Name) {
						continue
					}
					outSpaces = append(outSpaces, osOutSpace{ID: sp.GUID, Name: sp.Name})
				}
				// When a filter is active, omit orgs that have no remaining spaces.
				if (includePattern != "" || excludePattern != "") && len(outSpaces) == 0 {
					continue
				}
				outOrgs = append(outOrgs, osOutOrg{
					ID:     org.GUID,
					Name:   org.Name,
					Spaces: outSpaces,
				})
			}
			if len(outOrgs) > 0 {
				outRegions = append(outRegions, osOutRegion{ID: r.region, Orgs: outOrgs})
			}
		}

		doc := osOutDoc{Regions: outRegions}

		fmtLower := strings.ToLower(format)
		if fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0 {
			flatRows := flattenOrgSpacesRows(doc)
			if len(sortKeys) > 0 {
				sortRowsBy(flatRows, sortKeys)
			}
			switch fmtLower {
			case "json":
				out, err := json.MarshalIndent(flatDocValue(flatRows, fieldIDs), "", "  ")
				if err != nil {
					return fmt.Errorf("encoding JSON: %w", err)
				}
				fmt.Fprintln(os.Stdout, string(out))
				return nil
			case "csv":
				w := csv.NewWriter(os.Stdout)
				defer w.Flush()
				return writeFlatCSV(w, flatRows, fieldIDs)
			default: // toon
				out, err := toonenc.Marshal(flatDocValue(flatRows, fieldIDs), toonenc.WithIndent(2))
				if err != nil {
					return fmt.Errorf("encoding TOON: %w", err)
				}
				if _, err = os.Stdout.Write(out); err != nil {
					return err
				}
				_, err = fmt.Fprintln(os.Stdout)
				return err
			}
		}

		switch fmtLower {
		case "json":
			out, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding JSON: %w", err)
			}
			fmt.Fprintln(os.Stdout, string(out))

		default: // toon
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
		return nil
	},
}

func init() {
	orgSpacesCmd.GroupID = "cf-org"
	rootCmd.AddCommand(orgSpacesCmd)
	orgSpacesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgSpacesCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
	orgSpacesCmd.Flags().String("include", "", "Only include spaces where org_name or space_name contains this pattern (case-insensitive substring match)")
	orgSpacesCmd.Flags().String("exclude", "", "Exclude spaces where org_name or space_name contains this pattern (case-insensitive substring match)")
	orgSpacesCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (region,org_id,org_name,space_id,space_name; switches json/toon to the flat {rows:[...]} view too). Default: every column, in the normal nested json/toon structure")
	orgSpacesCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too")
}
