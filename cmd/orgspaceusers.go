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

type ospSpaceDetail struct {
	Space cf.Space
	Users []cf.CfUser
	Roles map[string][]string // userGUID → role types
}

type ospOrgDetail struct {
	Org    cf.Organization
	Users  []cf.CfUser
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
	ID    string    `json:"space_id"   toon:"space_id"`
	Name  string    `json:"space_name" toon:"space_name"`
	Users []outUser `json:"cfusers"    toon:"cfusers"`
}

type ospOutOrg struct {
	ID     string        `json:"org_id"   toon:"org_id"`
	Name   string        `json:"org_name" toon:"org_name"`
	Users  []outUser     `json:"cfusers"  toon:"cfusers"`
	Spaces []ospOutSpace `json:"spaces"   toon:"spaces"`
}

type ospOutRegion struct {
	ID   string      `json:"region" toon:"region"`
	Orgs []ospOutOrg `json:"orgs"   toon:"orgs"`
}

type ospOutDoc struct {
	Regions []ospOutRegion `json:"regions" toon:"regions"`
}

// ospFlatRow is the flat, one-row-per-user view of ospOutDoc — the same
// shape as the csv output (space_id/space_name are empty for org-level
// users). Used directly for csv, and for json/toon too when --fields or
// --sort-by is given (otherwise json/toon keep the nested region/org/space
// structure). Not used for uar.csv, whose columns are fixed.
type ospFlatRow struct {
	Region       string `json:"region"        toon:"region"`
	OrgID        string `json:"org_id"        toon:"org_id"`
	OrgName      string `json:"org_name"      toon:"org_name"`
	SpaceID      string `json:"space_id"      toon:"space_id"`
	SpaceName    string `json:"space_name"    toon:"space_name"`
	CFUserID     string `json:"cfuser_id"     toon:"cfuser_id"`
	CFUserName   string `json:"cfuser_name"   toon:"cfuser_name"`
	CFUserOrigin string `json:"cfuser_origin" toon:"cfuser_origin"`
	CFUserRoles  string `json:"cfuser_roles"  toon:"cfuser_roles"`
}

// flattenOspRows converts doc's nested region/org/space structure into one
// flat row per user (org-level users get empty SpaceID/SpaceName).
func flattenOspRows(doc ospOutDoc) []ospFlatRow {
	var rows []ospFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				rows = append(rows, ospFlatRow{
					Region: r.ID, OrgID: o.ID, OrgName: o.Name,
					CFUserID: u.ID, CFUserName: u.Name, CFUserOrigin: u.Origin, CFUserRoles: u.Roles,
				})
			}
			for _, sp := range o.Spaces {
				for _, u := range sp.Users {
					rows = append(rows, ospFlatRow{
						Region: r.ID, OrgID: o.ID, OrgName: o.Name, SpaceID: sp.ID, SpaceName: sp.Name,
						CFUserID: u.ID, CFUserName: u.Name, CFUserOrigin: u.Origin, CFUserRoles: u.Roles,
					})
				}
			}
		}
	}
	return rows
}

// buildOspOutputDoc converts raw fetch results into the shared output model.
// includePattern/excludePattern are optional comma-separated keyword lists
// matched case-insensitively against user id/name/origin/roles; spaces and
// orgs with no matching users are omitted.
func buildOspOutputDoc(results []ospRegionData, includePattern, excludePattern string) (ospOutDoc, []error) {
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
			for _, sd := range od.Spaces {
				sp := ospOutSpace{ID: sd.Space.GUID, Name: sd.Space.Name}
				for _, u := range sd.Users {
					ou := outUser{
						ID:     u.GUID,
						Name:   u.Username,
						Origin: u.Origin,
						Roles:  strings.Join(sd.Roles[u.GUID], ";"),
					}
					if userMatchesIncludeExclude(ou, includePattern, excludePattern) {
						sp.Users = append(sp.Users, ou)
					}
				}
				if len(sp.Users) > 0 {
					oo.Spaces = append(oo.Spaces, sp)
				}
			}
			if len(oo.Users) > 0 || len(oo.Spaces) > 0 {
				or.Orgs = append(or.Orgs, oo)
			}
		}
		if len(or.Orgs) > 0 {
			doc.Regions = append(doc.Regions, or)
		}
	}
	return doc, errs
}

// ── command ──────────────────────────────────────────────────────────────────

var orgSpaceUsersCmd = &cobra.Command{
	Use:   "org-space-users",
	Short: "List org and space users across every accessible organization",
	Long: `List users at the org and space level across one or more regions.

Output formats (--format):
  toon     Token-Oriented Object Notation — compact, human-readable (default)
  json     JSON document
  csv      CSV rows: region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles
           (space_id and space_name are empty for org-level users)
  uar.csv  User Access Review CSV, one row per org/space membership.
           Columns: Space/Org ID,Space/Org Name,Group Type,Member,Role
           Group Type is "Organization" or "Space"; Role lists all roles the
           member holds at that scope, comma-separated.

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
default toon applies (uar.csv is never auto-detected — pass --format
uar.csv explicitly). Pass --format explicitly to override the extension.

json/toon normally nest regions/orgs/spaces instead of the flat csv shape,
but switch to that same flat {"rows": [...]} view whenever --fields or
--sort-by is given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
csv columns above) to limit output to just those columns, in the order
given. Omit it to get every column. Ignored for uar.csv, whose columns are
fixed.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep users in their natural
(API) order. Ignored for uar.csv.

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
		fieldIDs, err := parseFieldsFlag[ospFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[ospFlatRow](sortByFlag, "--sort-by")
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
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = ospRegionData{Region: regionName, Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				allRoles, err := client.ListAllRoles(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not fetch roles for %s: %v\n", regionName, err)
					allRoles = cf.AllRoles{
						OrgRoles:   map[string]map[string][]string{},
						SpaceRoles: map[string]map[string][]string{},
					}
				}
				slog.Debug("roles fetched", "region", regionName)

				spacesByOrg, err := client.ListAllSpaces(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not fetch spaces for %s: %v\n", regionName, err)
					spacesByOrg = map[string][]cf.Space{}
				}
				slog.Debug("spaces fetched", "region", regionName)

				var orgDetails []ospOrgDetail
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

					var spaceDetails []ospSpaceDetail
					for _, space := range spacesByOrg[org.GUID] {
						spaceUsers, err := client.ListSpaceUsers(ctx, space.GUID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "warning: skipping space %q in org %q: %v\n", space.Name, org.Name, err)
							continue
						}
						spaceRoles := allRoles.SpaceRoles[space.GUID]
						if spaceRoles == nil {
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

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		fmtLower := strings.ToLower(format)
		if fmtLower != "uar.csv" && (fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0) {
			doc, errs := buildOspOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			flatRows := flattenOspRows(doc)
			if len(sortKeys) > 0 {
				sortRowsBy(flatRows, sortKeys)
			}
			switch fmtLower {
			case "json":
				return writeOspJSON(out, flatDocValue(flatRows, fieldIDs))
			case "csv":
				csvW := newRainbowCSVWriter(out, color)
				defer csvW.Flush()
				return writeFlatCSV(csvW, flatRows, fieldIDs)
			default:
				return writeOspToon(out, flatDocValue(flatRows, fieldIDs), color)
			}
		}

		switch fmtLower {
		case "json":
			doc, errs := buildOspOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeOspJSON(out, doc)
		case "uar.csv":
			return writeOspUARCSV(out, results, includePattern, excludePattern, color)
		default: // "toon"
			doc, errs := buildOspOutputDoc(results, includePattern, excludePattern)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
			return writeOspToon(out, doc, color)
		}
	},
}

// writeOspToon and writeOspJSON take val as either ospOutDoc (the default
// nested region/org/space structure) or flatDocValue's result (used
// instead when --fields or --sort-by is given) — both are just Go values
// that toonenc/json reflect over.
func writeOspToon(w io.Writer, val interface{}, color bool) error {
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

func writeOspJSON(w io.Writer, val interface{}) error {
	out, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

// writeOspUARCSV writes the uar.csv format: one row per org/space membership,
// columns Space/Org ID,Space/Org Name,Group Type,Member,Role.
func writeOspUARCSV(w io.Writer, results []ospRegionData, includePattern, excludePattern string, color bool) error {
	doc, errs := buildOspOutputDoc(results, includePattern, excludePattern)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	csvW := newRainbowCSVWriter(w, color)
	defer csvW.Flush()

	if err := csvW.Write([]string{"Space/Org ID", "Space/Org Name", "Group Type", "Member", "Role"}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				if err := csvW.Write([]string{o.ID, o.Name, "Organization", u.Name, ospUARRoles(u.Roles)}); err != nil {
					return err
				}
			}
			for _, sp := range o.Spaces {
				for _, u := range sp.Users {
					if err := csvW.Write([]string{sp.ID, sp.Name, "Space", u.Name, ospUARRoles(u.Roles)}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// ospUARRoles converts the ";"-joined role list used elsewhere in this
// command into the ", "-separated form expected by the uar.csv format.
func ospUARRoles(roles string) string {
	return strings.Join(strings.Split(roles, ";"), ", ")
}

func init() {
	orgSpaceUsersCmd.GroupID = "cf-org"
	rootCmd.AddCommand(orgSpaceUsersCmd)
	orgSpaceUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgSpaceUsersCmd.Flags().String("format", "toon", "Output format: toon (default), json, csv, or uar.csv; auto-detected from --output's extension (.json, .csv, or .toon — uar.csv is never auto-detected) when --format is not explicitly given")
	orgSpaceUsersCmd.Flags().String("include", "", "Only include users where id, name, origin, or roles contain any of these comma-separated, case-insensitive keywords")
	orgSpaceUsersCmd.Flags().String("exclude", "", "Exclude users where id, name, origin, or roles contain any of these comma-separated, case-insensitive keywords")
	orgSpaceUsersCmd.Flags().String("org", "", "Restrict to a single org by exact GUID")
	orgSpaceUsersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	orgSpaceUsersCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	orgSpaceUsersCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (same columns as csv; switches json/toon to the flat {rows:[...]} view too; ignored for uar.csv). Default: every column, in the normal nested json/toon structure")
	orgSpaceUsersCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too; ignored for uar.csv")
}
