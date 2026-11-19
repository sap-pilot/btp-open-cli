package cmd

import (
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

// spFlatRow is the flat, one-row-per-space view of spOutDoc — the same
// shape as the CSV output. Used directly for CSV, and for JSON/TOON too
// when --fields or --sort-by is given; otherwise JSON/TOON keep the nested
// region/org structure.
type spFlatRow struct {
	Region    string `json:"region"     toon:"region"`
	OrgID     string `json:"org_id"     toon:"org_id"`
	OrgName   string `json:"org_name"   toon:"org_name"`
	SpaceID   string `json:"space_id"   toon:"space_id"`
	SpaceName string `json:"space_name" toon:"space_name"`
}

// flattenSpacesRows converts doc's nested region/org structure into one
// flat row per space.
func flattenSpacesRows(doc spOutDoc) []spFlatRow {
	var rows []spFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				rows = append(rows, spFlatRow{Region: r.ID, OrgID: o.ID, OrgName: o.Name, SpaceID: sp.ID, SpaceName: sp.Name})
			}
		}
	}
	return rows
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

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.

json/toon normally nest regions/orgs/spaces instead of the flat csv shape,
but switch to that same flat {"rows": [...]} view whenever --fields or
--sort-by is given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
csv columns above) to limit output to just those columns, in the order
given. Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep the default order:
sorted by space_name within each org.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		outputFile, _ := cmd.Flags().GetString("output")
		format, _ := cmd.Flags().GetString("format")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs, err := parseFieldsFlag[spFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[spFlatRow](sortByFlag, "--sort-by")
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
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		fmtLower := strings.ToLower(format)
		if fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0 {
			doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			flatRows := flattenSpacesRows(doc)
			if len(sortKeys) > 0 {
				sortRowsBy(flatRows, sortKeys)
			}
			switch fmtLower {
			case "json":
				return writeSpacesJSON(out, flatDocValue(flatRows, fieldIDs))
			case "csv":
				csvW := newRainbowCSVWriter(out, color)
				defer csvW.Flush()
				return writeFlatCSV(csvW, flatRows, fieldIDs)
			default:
				return writeSpacesToon(out, flatDocValue(flatRows, fieldIDs), color)
			}
		}

		switch fmtLower {
		case "json":
			doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeSpacesJSON(out, doc)
		default: // "toon"
			doc, errs := buildSpacesDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeSpacesToon(out, doc, color)
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

// writeSpacesToon and writeSpacesJSON take val as either spOutDoc (the
// default nested region/org structure) or flatDocValue's result (used
// instead when --fields or --sort-by is given) — both are just Go values
// that toonenc/json reflect over.
func writeSpacesToon(w io.Writer, val interface{}, color bool) error {
	out, err := toonenc.Marshal(val, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	out = colorizeToon(out, color)
	if _, err = w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func writeSpacesJSON(w io.Writer, val interface{}) error {
	out, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
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
	spacesCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	spacesCmd.Flags().String("include", "", "Only include spaces where org_name or space_name contain any of these comma-separated keywords")
	spacesCmd.Flags().String("exclude", "", "Exclude spaces where org_name or space_name contain any of these comma-separated keywords")
	spacesCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (region,org_id,org_name,space_id,space_name; switches json/toon to the flat {rows:[...]} view too). Default: every column, in the normal nested json/toon structure")
	spacesCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too")
}
