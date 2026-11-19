package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// ── output types ─────────────────────────────────────────────────────────────

type rcOutRoleRef struct {
	RoleTemplateAppID string `json:"roleTemplateAppId" toon:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"  toon:"roleTemplateName"`
	Name              string `json:"role_name"         toon:"role_name"`
	Description       string `json:"description"       toon:"description"`
}

type rcOutRoleCollection struct {
	Name           string         `json:"rolecollection_name" toon:"rolecollection_name"`
	Description    string         `json:"description"         toon:"description"`
	IsReadOnly     bool           `json:"isReadOnly"          toon:"isReadOnly"`
	RoleReferences []rcOutRoleRef `json:"roleReferences"      toon:"roleReferences"`
}

type rcOutOrg struct {
	ID              string                `json:"org_id"          toon:"org_id"`
	Name            string                `json:"org_name"        toon:"org_name"`
	RoleCollections []rcOutRoleCollection `json:"roleCollections" toon:"roleCollections"`
}

type rcOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []rcOutOrg `json:"orgs"   toon:"orgs"`
}

type rcOutDoc struct {
	Regions []rcOutRegion `json:"regions" toon:"regions"`
}

// rcFlatRow is the flat, one-row-per-role-reference view of rcOutDoc — used
// for csv, and for json/toon too when --fields or --sort-by is given
// (otherwise json/toon keep the nested region/org/roleCollection
// structure). A role collection with no role references still gets one row,
// with the role_* columns empty.
type rcFlatRow struct {
	Region                    string `json:"region"                      toon:"region"`
	OrgID                     string `json:"org_id"                      toon:"org_id"`
	OrgName                   string `json:"org_name"                    toon:"org_name"`
	RoleCollectionName        string `json:"rolecollection_name"         toon:"rolecollection_name"`
	RoleCollectionDescription string `json:"rolecollection_description" toon:"rolecollection_description"`
	RoleCollectionIsReadOnly  bool   `json:"rolecollection_isReadOnly"   toon:"rolecollection_isReadOnly"`
	RoleTemplateAppID         string `json:"role_template_app_id"       toon:"role_template_app_id"`
	RoleTemplateName          string `json:"role_template_name"         toon:"role_template_name"`
	RoleName                  string `json:"role_name"                  toon:"role_name"`
	RoleDescription           string `json:"role_description"           toon:"role_description"`
}

// rcRowMatchesIncludeExclude applies --include/--exclude keyword filtering
// (comma-separated, case-insensitive, matched if any keyword is a substring
// of any field) to one role-collection/role-reference row's own fields:
// rolecollection_name, rolecollection_description, role_template_app_id,
// role_template_name, role_name, role_description.
func rcRowMatchesIncludeExclude(row rcFlatRow, includePattern, excludePattern string) bool {
	fields := []string{
		row.RoleCollectionName, row.RoleCollectionDescription,
		row.RoleTemplateAppID, row.RoleTemplateName, row.RoleName, row.RoleDescription,
	}
	if includePattern != "" && !skipMatches(includePattern, fields...) {
		return false
	}
	if excludePattern != "" && skipMatches(excludePattern, fields...) {
		return false
	}
	return true
}

// flattenRoleCollectionsRows converts doc's nested region/org/roleCollection
// structure into one flat row per role reference.
func flattenRoleCollectionsRows(doc rcOutDoc) []rcFlatRow {
	var rows []rcFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, rc := range o.RoleCollections {
				base := rcFlatRow{
					Region: r.ID, OrgID: o.ID, OrgName: o.Name,
					RoleCollectionName: rc.Name, RoleCollectionDescription: rc.Description,
					RoleCollectionIsReadOnly: rc.IsReadOnly,
				}
				if len(rc.RoleReferences) == 0 {
					rows = append(rows, base)
					continue
				}
				for _, ref := range rc.RoleReferences {
					row := base
					row.RoleTemplateAppID = ref.RoleTemplateAppID
					row.RoleTemplateName = ref.RoleTemplateName
					row.RoleName = ref.Name
					row.RoleDescription = ref.Description
					rows = append(rows, row)
				}
			}
		}
	}
	return rows
}

// ── command ───────────────────────────────────────────────────────────────────

var roleCollectionsCmd = &cobra.Command{
	Use:   "role-collections [role_collection_name_pattern]",
	Short: "List XSUAA role collections across all accessible organizations",
	Long: `List role collections (with their role references) from the XSUAA
Authorization API across one or more regions and organizations. To export
roles themselves (not role collections), use 'bo roles' instead.

<role_collection_name_pattern> is optional — a comma-separated list of
patterns, e.g. 'Custom_*,temp-*' or '*' (the default when omitted: every
role collection). Each pattern is a glob (via filepath.Match) if it contains
* ? [, otherwise a case-insensitive substring; a pattern prefixed with '!'
reverses it, and a role collection is selected if any pattern — negated or
not — matches it. Only role collections matching the pattern are included
in the output.

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually (suppress with
--no-prompt to skip the org silently instead).

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.

csv is always a flat list, one row per role reference (a role collection
with no role references still gets one row, with the role_* columns empty),
with columns: region, org_id, org_name, rolecollection_name,
rolecollection_description, rolecollection_isReadOnly, role_template_app_id,
role_template_name, role_name, role_description. json/toon normally nest
regions/orgs/roleCollections/roleReferences instead, but switch to that same
flat {"rows": [...]} view whenever --fields or --sort-by is given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
columns above) to limit output to just those columns, in the order given.
Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep the default order:
role collections sorted by name (role references within a role collection
keep their own default order: role_template_app_id then role_template_name).

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a row (one role reference, or the
role collection itself if it has none) matches if rolecollection_name,
rolecollection_description, role_template_app_id, role_template_name,
role_name, or role_description contains any of them (case-insensitive). A
role collection is dropped entirely once it has no matching row left.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_collection_name_pattern cannot be empty")
			}
		}
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgFilter, _ := cmd.Flags().GetString("org")
		outputFile, _ := cmd.Flags().GetString("output")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
		format, _ := cmd.Flags().GetString("format")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs, err := parseFieldsFlag[rcFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[rcFlatRow](sortByFlag, "--sort-by")
		if err != nil {
			return err
		}
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

		// When --org is specified, scan all regions to find the matching org(s)
		// by exact GUID or case-insensitive name substring, then scope
		// resolveXsuaaClients to only those orgs so other regions are not touched.
		if orgFilter != "" {
			fl := strings.ToLower(orgFilter)
			for _, apiURL := range apiURLs {
				tok, ok := creds.Tokens[apiURL]
				if !ok {
					continue
				}
				regionName := store.APIURLToRegion(apiURL)
				cfClient := cf.NewClient(apiURL, tok.AccessToken)
				cfClient.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
				orgs, listErr := cfClient.ListOrganizations(ctx)
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, listErr)
					continue
				}
				for _, org := range orgs {
					if strings.EqualFold(org.GUID, orgFilter) ||
						strings.Contains(strings.ToLower(org.Name), fl) {
						includeOrgs = append(includeOrgs, cosOrgRef{ID: org.GUID})
					}
				}
			}
			if len(includeOrgs) == 0 {
				return fmt.Errorf("org %q not found in any accessible region", orgFilter)
			}
		} else if orgsFile == "" {
			includeOrgs, err = resolveDefaultOrgScope(creds)
			if err != nil {
				return err
			}
		}

		// Phase 1: resolve XSUAA tokens for the target orgs.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
		if err != nil {
			return err
		}

		// Phase 2: fetch role collections per org in parallel.
		type orgResult struct {
			regionName      string
			orgGUID         string
			orgName         string
			roleCollections []xsuaa.RoleCollection
			err             error
		}
		results := make([]orgResult, len(clients))
		var wg sync.WaitGroup

		for i, w := range clients {
			wg.Add(1)
			go func(idx int, w xsuaaOrgClient) {
				defer wg.Done()
				slog.Debug("fetching XSUAA role collections", "region", w.RegionName, "org", w.OrgName)

				rcs, err := xsuaa.ListRoleCollections(ctx, w.APIURL, w.Token)
				if err != nil {
					results[idx] = orgResult{regionName: w.RegionName, orgGUID: w.OrgGUID, orgName: w.OrgName,
						err: fmt.Errorf("listing role collections: %w", err)}
					return
				}
				results[idx] = orgResult{
					regionName:      w.RegionName,
					orgGUID:         w.OrgGUID,
					orgName:         w.OrgName,
					roleCollections: rcs,
				}
			}(i, w)
		}
		wg.Wait()

		// Phase 3: assemble output, preserving region order.
		regionOrder := make([]string, 0)
		regionSeen := make(map[string]bool)
		for _, c := range clients {
			if !regionSeen[c.RegionName] {
				regionOrder = append(regionOrder, c.RegionName)
				regionSeen[c.RegionName] = true
			}
		}

		regionOrgs := make(map[string][]rcOutOrg)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.orgName, r.err)
				continue
			}

			var outRCs []rcOutRoleCollection
			for _, rc := range r.roleCollections {
				if !matchesNegatablePattern(pattern, rc.Name) {
					continue
				}
				var refs []rcOutRoleRef
				for _, ref := range rc.RoleReferences {
					candidate := rcFlatRow{
						RoleCollectionName: rc.Name, RoleCollectionDescription: rc.Description,
						RoleTemplateAppID: ref.RoleTemplateAppID, RoleTemplateName: ref.RoleTemplateName,
						RoleName: ref.Name, RoleDescription: ref.Description,
					}
					if !rcRowMatchesIncludeExclude(candidate, includePattern, excludePattern) {
						continue
					}
					refs = append(refs, rcOutRoleRef{
						RoleTemplateAppID: ref.RoleTemplateAppID,
						RoleTemplateName:  ref.RoleTemplateName,
						Name:              ref.Name,
						Description:       ref.Description,
					})
				}
				sort.Slice(refs, func(i, j int) bool {
					if refs[i].RoleTemplateAppID != refs[j].RoleTemplateAppID {
						return refs[i].RoleTemplateAppID < refs[j].RoleTemplateAppID
					}
					return refs[i].RoleTemplateName < refs[j].RoleTemplateName
				})
				if len(rc.RoleReferences) == 0 {
					// No references at all: keep/drop based on the role
					// collection's own fields alone (matching
					// flattenRoleCollectionsRows' "one empty row" case).
					base := rcFlatRow{RoleCollectionName: rc.Name, RoleCollectionDescription: rc.Description}
					if !rcRowMatchesIncludeExclude(base, includePattern, excludePattern) {
						continue
					}
				} else if (includePattern != "" || excludePattern != "") && len(refs) == 0 {
					continue // had references, but none matched
				}
				outRCs = append(outRCs, rcOutRoleCollection{
					Name:           rc.Name,
					Description:    rc.Description,
					IsReadOnly:     rc.IsReadOnly,
					RoleReferences: refs,
				})
			}
			sort.Slice(outRCs, func(i, j int) bool {
				return outRCs[i].Name < outRCs[j].Name
			})

			regionOrgs[r.regionName] = append(regionOrgs[r.regionName], rcOutOrg{
				ID:              r.orgGUID,
				Name:            r.orgName,
				RoleCollections: outRCs,
			})
		}

		var outRegions []rcOutRegion
		for _, rid := range regionOrder {
			orgs := regionOrgs[rid]
			if len(orgs) > 0 {
				outRegions = append(outRegions, rcOutRegion{ID: rid, Orgs: orgs})
			}
		}

		doc := rcOutDoc{Regions: outRegions}

		w, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		fmtLower := strings.ToLower(format)
		if fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0 {
			flatRows := flattenRoleCollectionsRows(doc)
			if len(sortKeys) > 0 {
				sortRowsBy(flatRows, sortKeys)
			}
			switch fmtLower {
			case "json":
				out, err := json.MarshalIndent(flatDocValue(flatRows, fieldIDs), "", "  ")
				if err != nil {
					return fmt.Errorf("encoding JSON: %w", err)
				}
				fmt.Fprintln(w, string(out))
				return nil
			case "csv":
				csvW := newRainbowCSVWriter(w, color)
				defer csvW.Flush()
				return writeFlatCSV(csvW, flatRows, fieldIDs)
			default: // toon
				out, err := toonenc.Marshal(flatDocValue(flatRows, fieldIDs), toonenc.WithIndent(2))
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
		}

		switch fmtLower {
		case "json":
			out, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding JSON: %w", err)
			}
			fmt.Fprintln(w, string(out))
		default: // toon
			out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
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
		return nil
	},
}

func init() {
	roleCollectionsCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(roleCollectionsCmd)
	roleCollectionsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	roleCollectionsCmd.Flags().String("org", "", "Org name or GUID to target (case-insensitive substring match on name, exact on GUID)")
	roleCollectionsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	roleCollectionsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	roleCollectionsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	roleCollectionsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
	roleCollectionsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	roleCollectionsCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (same columns as csv; switches json/toon to the flat {rows:[...]} view too). Default: every column, in the normal nested json/toon structure")
	roleCollectionsCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too")
	roleCollectionsCmd.Flags().String("include", "", "Only include rows where rolecollection_name, rolecollection_description, role_template_app_id, role_template_name, role_name, or role_description contain any of these comma-separated, case-insensitive keywords")
	roleCollectionsCmd.Flags().String("exclude", "", "Exclude rows where rolecollection_name, rolecollection_description, role_template_app_id, role_template_name, role_name, or role_description contain any of these comma-separated, case-insensitive keywords")
}
