package cmd

import (
	"encoding/json"
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
	ID     string `json:"cfuser_id"     toon:"cfuser_id"`
	Name   string `json:"cfuser_name"   toon:"cfuser_name"`
	Origin string `json:"cfuser_origin" toon:"cfuser_origin"`
	Roles  string `json:"cfuser_roles"  toon:"cfuser_roles"`
}

type outOrg struct {
	ID    string    `json:"org_id"   toon:"org_id"`
	Name  string    `json:"org_name" toon:"org_name"`
	Users []outUser `json:"cfusers"  toon:"cfusers"`
}

type outRegion struct {
	ID   string   `json:"region" toon:"region"`
	Orgs []outOrg `json:"orgs"   toon:"orgs"`
}

type outDoc struct {
	Regions []outRegion `json:"regions" toon:"regions"`
}

// ouFlatRow is the flat, one-row-per-user view of outDoc — the same shape
// as the CSV output. Used directly for CSV, and for JSON/TOON too when
// --fields or --sort-by is given; otherwise JSON/TOON keep the nested
// region/org structure.
type ouFlatRow struct {
	Region       string `json:"region"        toon:"region"`
	OrgID        string `json:"org_id"        toon:"org_id"`
	OrgName      string `json:"org_name"      toon:"org_name"`
	CFUserID     string `json:"cfuser_id"     toon:"cfuser_id"`
	CFUserName   string `json:"cfuser_name"   toon:"cfuser_name"`
	CFUserOrigin string `json:"cfuser_origin" toon:"cfuser_origin"`
	CFUserRoles  string `json:"cfuser_roles"  toon:"cfuser_roles"`
}

// flattenOrgUsersRows converts doc's nested region/org structure into one
// flat row per user.
func flattenOrgUsersRows(doc outDoc) []ouFlatRow {
	var rows []ouFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				rows = append(rows, ouFlatRow{
					Region: r.ID, OrgID: o.ID, OrgName: o.Name,
					CFUserID: u.ID, CFUserName: u.Name, CFUserOrigin: u.Origin, CFUserRoles: u.Roles,
				})
			}
		}
	}
	return rows
}

// userMatchesIncludeExclude applies --include/--exclude keyword filtering
// (comma-separated, case-insensitive, matched if any keyword is a substring
// of any field) against a user's id, name, origin, and roles.
func userMatchesIncludeExclude(u outUser, includePattern, excludePattern string) bool {
	fields := []string{u.ID, u.Name, u.Origin, u.Roles}
	if includePattern != "" && !skipMatches(includePattern, fields...) {
		return false
	}
	if excludePattern != "" && skipMatches(excludePattern, fields...) {
		return false
	}
	return true
}

// buildOutputDoc converts raw fetch results into the shared output model.
// includePattern/excludePattern are optional comma-separated keyword lists
// applied to user id/name/origin/roles; orgs and regions with no matching
// users are omitted from the result.
func buildOutputDoc(results []regionData, includePattern, excludePattern string) (outDoc, []error) {
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
				if userMatchesIncludeExclude(ou, includePattern, excludePattern) {
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
  csv   CSV rows: region,org_id,org_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles

Use --org to scope to a single org by GUID, or --orgs to provide a CSV
file (columns: region,org_id,org_name) listing the orgs to include.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a user matches if id, name, origin, or
roles contains any of them (case-insensitive).

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.

csv is always a flat list, one row per user, with columns:
region,org_id,org_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles.
json/toon normally nest regions/orgs/cfusers instead, but switch to that
same flat {"rows": [...]} view whenever --fields or --sort-by is given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
columns above) to limit output to just those columns, in the order given.
Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep users in their natural
(API) order.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		orgGUID, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		outputFile, _ := cmd.Flags().GetString("output")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs, err := parseFieldsFlag[ouFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[ouFlatRow](sortByFlag, "--sort-by")
		if err != nil {
			return err
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		// Parse --orgs CSV if provided.
		var includeOrgs cosOrgSet
		if orgsFile != "" {
			includeOrgs, err = parseCosOrgCSV(orgsFile)
			if err != nil {
				return fmt.Errorf("invalid --orgs CSV: %w", err)
			}
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

		if orgGUID == "" && orgsFile == "" {
			includeOrgs, err = resolveDefaultOrgScope(creds)
			if err != nil {
				return err
			}
		}

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
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

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
					if orgGUID != "" && org.GUID != orgGUID {
						continue
					}
					if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
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

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		fmtLower := strings.ToLower(format)
		if fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0 {
			doc, errs := buildOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			flatRows := flattenOrgUsersRows(doc)
			if len(sortKeys) > 0 {
				sortRowsBy(flatRows, sortKeys)
			}
			switch fmtLower {
			case "json":
				return writeOrgUsersJSON(out, flatDocValue(flatRows, fieldIDs))
			case "csv":
				csvW := newRainbowCSVWriter(out, color)
				defer csvW.Flush()
				return writeFlatCSV(csvW, flatRows, fieldIDs)
			default:
				return writeOrgUsersToon(out, flatDocValue(flatRows, fieldIDs), color)
			}
		}

		switch fmtLower {
		case "json":
			doc, errs := buildOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeOrgUsersJSON(out, doc)
		default: // "toon"
			doc, errs := buildOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeOrgUsersToon(out, doc, color)
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
//
// writeOrgUsersToon and writeOrgUsersJSON take val as either outDoc (the
// default nested region/org structure) or flatDocValue's result (used
// instead when --fields or --sort-by is given) — both are just Go values
// that toonenc/json reflect over.
func writeOrgUsersToon(w io.Writer, val interface{}, color bool) error {
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

// writeOrgUsersJSON serializes val as indented JSON.
func writeOrgUsersJSON(w io.Writer, val interface{}) error {
	out, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func init() {
	orgUsersCmd.GroupID = "cf-org"
	rootCmd.AddCommand(orgUsersCmd)
	orgUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgUsersCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	orgUsersCmd.Flags().String("include", "", "Only include users where id, name, origin, or roles contain any of these comma-separated, case-insensitive keywords")
	orgUsersCmd.Flags().String("exclude", "", "Exclude users where id, name, origin, or roles contain any of these comma-separated, case-insensitive keywords")
	orgUsersCmd.Flags().String("org", "", "Restrict to a single org by exact GUID")
	orgUsersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	orgUsersCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	orgUsersCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (same columns as csv; switches json/toon to the flat {rows:[...]} view too). Default: every column, in the normal nested json/toon structure")
	orgUsersCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too")
}
