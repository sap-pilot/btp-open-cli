package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
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

// ── output types ──────────────────────────────────────────────────────────────

type spOutSpace struct {
	ID   string `json:"space_id"   toon:"space_id"`
	Name string `json:"space_name" toon:"space_name"`
}

type spOutOrg struct {
	ID     string       `json:"org_id"   toon:"org_id"`
	Name   string       `json:"org_name" toon:"org_name"`
	Spaces []spOutSpace `json:"spaces"   toon:"spaces"`
}

type spOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []spOutOrg `json:"orgs"   toon:"orgs"`
}

type spOutDoc struct {
	Regions []spOutRegion `json:"regions" toon:"regions"`
}

// ── fetch result ──────────────────────────────────────────────────────────────

type spacesRegionResult struct {
	Region string
	Orgs   []cf.Organization
	Spaces map[string][]cf.Space // orgGUID → spaces
	Err    error
}

// ── command ───────────────────────────────────────────────────────────────────

var spacesCmd = &cobra.Command{
	Use:   "spaces",
	Short: "List all spaces in the selected or specified orgs",
	Long: `List Cloud Foundry spaces, grouped by org and region.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows: region,org_id,org_name,space_id,space_name

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a space matches if org_name or
space_name contains any of them (case-insensitive).

Use --output/-o to write the result to a file instead of stdout.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		outputFile, _ := cmd.Flags().GetString("output")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")

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

		if orgGUID == "" && orgsFile == "" {
			includeOrgs, err = resolveDefaultOrgScope(creds)
			if err != nil {
				return err
			}
		}

		results := make([]spacesRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = spacesRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = spacesRegionResult{Region: regionName,
						Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}

				var filteredOrgs []cf.Organization
				for _, org := range orgs {
					if orgGUID != "" && org.GUID != orgGUID {
						continue
					}
					if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					filteredOrgs = append(filteredOrgs, org)
				}
				if len(filteredOrgs) == 0 {
					results[idx] = spacesRegionResult{Region: regionName}
					return
				}

				orgGUIDs := make([]string, len(filteredOrgs))
				for i, o := range filteredOrgs {
					orgGUIDs[i] = o.GUID
				}

				spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
				if err != nil {
					results[idx] = spacesRegionResult{Region: regionName,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}

				bySpace := make(map[string][]cf.Space)
				for _, sp := range spaces {
					orgGUID := sp.Relationships.Organization.Data.GUID
					bySpace[orgGUID] = append(bySpace[orgGUID], sp)
				}

				results[idx] = spacesRegionResult{Region: regionName, Orgs: filteredOrgs, Spaces: bySpace}
			}(i, apiURL)
		}
		wg.Wait()

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			return writeSpacesJSON(out, results, includePattern, excludePattern)
		case "csv":
			return writeSpacesCSV(out, results, includePattern, excludePattern)
		default: // "toon"
			return writeSpacesToon(out, results, includePattern, excludePattern)
		}
	},
}

// ── output builders ───────────────────────────────────────────────────────────

func buildSpacesDoc(results []spacesRegionResult, includePattern, excludePattern string) (spOutDoc, []error) {
	var doc spOutDoc
	var errs []error

	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", r.Region, r.Err))
			continue
		}

		var outOrgs []spOutOrg
		for _, org := range r.Orgs {
			spaces := r.Spaces[org.GUID]
			sort.Slice(spaces, func(i, j int) bool { return spaces[i].Name < spaces[j].Name })

			var outSpaces []spOutSpace
			for _, sp := range spaces {
				if includePattern != "" && !skipMatches(includePattern, org.Name, sp.Name) {
					continue
				}
				if excludePattern != "" && skipMatches(excludePattern, org.Name, sp.Name) {
					continue
				}
				outSpaces = append(outSpaces, spOutSpace{ID: sp.GUID, Name: sp.Name})
			}
			// When a filter is active, omit orgs that have no remaining spaces.
			if (includePattern != "" || excludePattern != "") && len(outSpaces) == 0 {
				continue
			}
			outOrgs = append(outOrgs, spOutOrg{ID: org.GUID, Name: org.Name, Spaces: outSpaces})
		}
		if len(outOrgs) > 0 {
			doc.Regions = append(doc.Regions, spOutRegion{ID: r.Region, Orgs: outOrgs})
		}
	}
	return doc, errs
}

func writeSpacesToon(w io.Writer, results []spacesRegionResult, includePattern, excludePattern string) error {
	doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err = w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func writeSpacesJSON(w io.Writer, results []spacesRegionResult, includePattern, excludePattern string) error {
	doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func writeSpacesCSV(w io.Writer, results []spacesRegionResult, includePattern, excludePattern string) error {
	doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	csvW := csv.NewWriter(w)
	defer csvW.Flush()
	if err := csvW.Write([]string{"region", "org_id", "org_name", "space_id", "space_name"}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				if err := csvW.Write([]string{r.ID, o.ID, o.Name, sp.ID, sp.Name}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func init() {
	spacesCmd.GroupID = "cf-org"
	rootCmd.AddCommand(spacesCmd)
	spacesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	spacesCmd.Flags().String("org", "", "Org GUID to target; only spaces from this org will be listed")
	spacesCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	spacesCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	spacesCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	spacesCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
	spacesCmd.Flags().String("include", "", "Only include spaces where org_name or space_name contain any of these comma-separated keywords")
	spacesCmd.Flags().String("exclude", "", "Exclude spaces where org_name or space_name contain any of these comma-separated keywords")
}
