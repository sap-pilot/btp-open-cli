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

type roOutRoleAttr struct {
	AttributeName        string   `json:"attributeName"                   toon:"attributeName"`
	AttributeValueOrigin string   `json:"attributeValueOrigin,omitempty"   toon:"attributeValueOrigin,omitempty"`
	AttributeValues      []string `json:"attributeValues"                 toon:"attributeValues"`
	Description          string   `json:"description,omitempty"           toon:"description,omitempty"`
	ValueRequired        bool     `json:"valueRequired,omitempty"         toon:"valueRequired,omitempty"`
}

type roOutRole struct {
	RoleTemplateAppID string          `json:"roleTemplateAppId"       toon:"roleTemplateAppId"`
	RoleTemplateName  string          `json:"roleTemplateName"        toon:"roleTemplateName"`
	Name              string          `json:"role_name"               toon:"role_name"`
	AppName           string          `json:"appName"                 toon:"appName"`
	Description       string          `json:"description"             toon:"description"`
	IsReadOnly        bool            `json:"isReadOnly"              toon:"isReadOnly"`
	AttributeList     []roOutRoleAttr `json:"attributeList,omitempty" toon:"attributeList,omitempty"`
}

type roOutOrg struct {
	ID    string      `json:"org_id"   toon:"org_id"`
	Name  string      `json:"org_name" toon:"org_name"`
	Roles []roOutRole `json:"roles"    toon:"roles"`
}

type roOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []roOutOrg `json:"orgs"   toon:"orgs"`
}

type roOutDoc struct {
	Regions []roOutRegion `json:"regions" toon:"regions"`
}

// roFlatRow is the flat, one-row-per-role view of roOutDoc — used for csv,
// and for json/toon too when --fields or --sort-by is given (otherwise
// json/toon keep the nested region/org structure, since that's the exact
// shape 'bo create-roles'/'bo update-roles'/'bo delete-roles --roles' read
// back in). AttributeList is encoded as a JSON string (empty when the role
// has none), the same way apps.go encodes app_env for its CSV.
type roFlatRow struct {
	Region            string `json:"region"            toon:"region"`
	OrgID             string `json:"org_id"            toon:"org_id"`
	OrgName           string `json:"org_name"          toon:"org_name"`
	RoleTemplateAppID string `json:"roleTemplateAppId" toon:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"  toon:"roleTemplateName"`
	RoleName          string `json:"role_name"         toon:"role_name"`
	AppName           string `json:"appName"           toon:"appName"`
	Description       string `json:"description"       toon:"description"`
	IsReadOnly        bool   `json:"isReadOnly"        toon:"isReadOnly"`
	AttributeList     string `json:"attributeList"     toon:"attributeList"`
}

// flattenRolesRows converts doc's nested region/org structure into one flat
// row per role.
// roMatchesIncludeExclude applies --include/--exclude keyword filtering
// (comma-separated, case-insensitive, matched if any keyword is a substring
// of any field) against a role's roleTemplateAppId, roleTemplateName,
// role_name, appName, and description.
func roMatchesIncludeExclude(role xsuaa.Role, includePattern, excludePattern string) bool {
	fields := []string{role.RoleTemplateAppID, role.RoleTemplateName, role.Name, role.AppName, role.Description}
	if includePattern != "" && !skipMatches(includePattern, fields...) {
		return false
	}
	if excludePattern != "" && skipMatches(excludePattern, fields...) {
		return false
	}
	return true
}

func flattenRolesRows(doc roOutDoc) ([]roFlatRow, error) {
	var rows []roFlatRow
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, role := range o.Roles {
				var attrsCol string
				if len(role.AttributeList) > 0 {
					attrsJSON, err := json.Marshal(role.AttributeList)
					if err != nil {
						return nil, fmt.Errorf("encoding attributeList for %q: %w", role.Name, err)
					}
					attrsCol = string(attrsJSON)
				}
				rows = append(rows, roFlatRow{
					Region: r.ID, OrgID: o.ID, OrgName: o.Name,
					RoleTemplateAppID: role.RoleTemplateAppID, RoleTemplateName: role.RoleTemplateName,
					RoleName: role.Name, AppName: role.AppName, Description: role.Description,
					IsReadOnly: role.IsReadOnly, AttributeList: attrsCol,
				})
			}
		}
	}
	return rows, nil
}

func xsuaaRoleToOut(role xsuaa.Role) roOutRole {
	var attrs []roOutRoleAttr
	for _, a := range role.AttributeList {
		attrs = append(attrs, roOutRoleAttr{
			AttributeName:        a.AttributeName,
			AttributeValueOrigin: a.AttributeValueOrigin,
			AttributeValues:      a.AttributeValues,
			Description:          a.Description,
			ValueRequired:        a.ValueRequired,
		})
	}
	return roOutRole{
		RoleTemplateAppID: role.RoleTemplateAppID,
		RoleTemplateName:  role.RoleTemplateName,
		Name:              role.Name,
		AppName:           role.AppName,
		Description:       role.Description,
		IsReadOnly:        role.IsReadOnly,
		AttributeList:     attrs,
	}
}

func roOutToXsuaaRole(role roOutRole) xsuaa.Role {
	var attrs []xsuaa.RoleAttribute
	for _, a := range role.AttributeList {
		attrs = append(attrs, xsuaa.RoleAttribute{
			AttributeName:        a.AttributeName,
			AttributeValueOrigin: a.AttributeValueOrigin,
			AttributeValues:      a.AttributeValues,
			Description:          a.Description,
			ValueRequired:        a.ValueRequired,
		})
	}
	return xsuaa.Role{
		RoleTemplateAppID: role.RoleTemplateAppID,
		RoleTemplateName:  role.RoleTemplateName,
		Name:              role.Name,
		AppName:           role.AppName,
		Description:       role.Description,
		IsReadOnly:        role.IsReadOnly,
		AttributeList:     attrs,
	}
}

// ── command ───────────────────────────────────────────────────────────────────

var rolesCmd = &cobra.Command{
	Use:   "roles [role_name_pattern]",
	Short: "Export XSUAA roles (with attributes) across all accessible organizations",
	Long: `Export roles, including their attribute lists, from the XSUAA Authorization
API across one or more regions and organizations.

<role_name_pattern> is optional — a comma-separated list of patterns, e.g.
'Custom_*,temp-*' or '*' (the default when omitted: every role). Each
pattern is a glob (via filepath.Match) if it contains * ? [, otherwise a
case-insensitive substring; a pattern prefixed with '!' reverses it, and a
role is selected if any pattern — negated or not — matches it. Only roles
matching the pattern are included in the output.

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually (suppress with
--no-prompt to skip the org silently instead).

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --output/-o to write the result to a file instead of stdout. The written
JSON or TOON file (in its default, nested form — see below) can be fed back
into 'bo create-roles', 'bo update-roles', or 'bo delete-roles --roles
<file>'. If --format is not explicitly given, it is auto-detected from
--output's extension: .json selects json, .csv selects csv, .toon selects
toon — otherwise the default toon applies. Pass --format explicitly to
override the extension.

csv is always a flat list, one row per role, with columns: region, org_id,
org_name, roleTemplateAppId, roleTemplateName, role_name, appName,
description, isReadOnly, attributeList (the role's attribute list, JSON-
encoded, empty when it has none). json/toon normally nest
regions/orgs/roles instead (the shape the other 'bo *-roles' commands read
back in), but switch to that same flat {"rows": [...]} view — not suitable
for feeding back into those commands — whenever --fields or --sort-by is
given.

Use --fields <column,...> (case-insensitive, comma-separated, any of the
columns above) to limit output to just those columns, in the order given.
Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order, each optionally suffixed with ":desc" for
descending (":asc" is the default). Omit it to keep the default order:
sorted by roleTemplateAppId then roleTemplateName.

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a role matches if roleTemplateAppId,
roleTemplateName, role_name, appName, or description contains any of them
(case-insensitive).

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_name_pattern cannot be empty")
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
		fieldIDs, err := parseFieldsFlag[roFlatRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[roFlatRow](sortByFlag, "--sort-by")
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

		// Phase 2: fetch roles per org in parallel.
		type orgResult struct {
			regionName string
			orgGUID    string
			orgName    string
			roles      []xsuaa.Role
			err        error
		}
		results := make([]orgResult, len(clients))
		var wg sync.WaitGroup

		for i, w := range clients {
			wg.Add(1)
			go func(idx int, w xsuaaOrgClient) {
				defer wg.Done()
				slog.Debug("fetching XSUAA roles", "region", w.RegionName, "org", w.OrgName)

				roles, err := xsuaa.ListRoles(ctx, w.APIURL, w.Token)
				if err != nil {
					results[idx] = orgResult{regionName: w.RegionName, orgGUID: w.OrgGUID, orgName: w.OrgName,
						err: fmt.Errorf("listing roles: %w", err)}
					return
				}
				results[idx] = orgResult{
					regionName: w.RegionName,
					orgGUID:    w.OrgGUID,
					orgName:    w.OrgName,
					roles:      roles,
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

		regionOrgs := make(map[string][]roOutOrg)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.orgName, r.err)
				continue
			}

			var outRoles []roOutRole
			for _, role := range r.roles {
				if !matchesNegatablePattern(pattern, role.Name) {
					continue
				}
				if !roMatchesIncludeExclude(role, includePattern, excludePattern) {
					continue
				}
				outRoles = append(outRoles, xsuaaRoleToOut(role))
			}
			sort.Slice(outRoles, func(i, j int) bool {
				if outRoles[i].RoleTemplateAppID != outRoles[j].RoleTemplateAppID {
					return outRoles[i].RoleTemplateAppID < outRoles[j].RoleTemplateAppID
				}
				return outRoles[i].RoleTemplateName < outRoles[j].RoleTemplateName
			})

			regionOrgs[r.regionName] = append(regionOrgs[r.regionName], roOutOrg{
				ID:    r.orgGUID,
				Name:  r.orgName,
				Roles: outRoles,
			})
		}

		var outRegions []roOutRegion
		for _, rid := range regionOrder {
			orgs := regionOrgs[rid]
			if len(orgs) > 0 {
				outRegions = append(outRegions, roOutRegion{ID: rid, Orgs: orgs})
			}
		}

		doc := roOutDoc{Regions: outRegions}

		w, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		fmtLower := strings.ToLower(format)
		if fmtLower == "csv" || len(fieldIDs) > 0 || len(sortKeys) > 0 {
			flatRows, flatErr := flattenRolesRows(doc)
			if flatErr != nil {
				return flatErr
			}
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
	rolesCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(rolesCmd)
	rolesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	rolesCmd.Flags().String("org", "", "Org name or GUID to target (case-insensitive substring match on name, exact on GUID)")
	rolesCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	rolesCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	rolesCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	rolesCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
	rolesCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	rolesCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (same columns as csv; switches json/toon to the flat {rows:[...]} view too). Default: every column, in the normal nested json/toon structure")
	rolesCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to the flat {rows:[...]} view too")
	rolesCmd.Flags().String("include", "", "Only include roles where roleTemplateAppId, roleTemplateName, role_name, appName, or description contain any of these comma-separated, case-insensitive keywords")
	rolesCmd.Flags().String("exclude", "", "Exclude roles where roleTemplateAppId, roleTemplateName, role_name, appName, or description contain any of these comma-separated, case-insensitive keywords")
}
