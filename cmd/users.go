package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// ── output types ─────────────────────────────────────────────────────────────

type usrOutUser struct {
	ID            string `json:"user_id"         toon:"user_id"`
	ExternalID    string `json:"user_externalId"  toon:"user_externalId"`
	Origin        string `json:"user_origin"      toon:"user_origin"`
	UserName      string `json:"user_name"        toon:"user_name"`
	Email         string `json:"email"            toon:"email"`
	LastLogonTime string `json:"lastLogonTime"    toon:"lastLogonTime"`
	Groups        string `json:"groups"           toon:"groups"`
}

type usrOutOrg struct {
	ID    string       `json:"org_id"   toon:"org_id"`
	Name  string       `json:"org_name" toon:"org_name"`
	Users []usrOutUser `json:"users"    toon:"users"`
}

type usrOutRegion struct {
	ID   string      `json:"region" toon:"region"`
	Orgs []usrOutOrg `json:"orgs"   toon:"orgs"`
}

type usrOutDoc struct {
	Regions []usrOutRegion `json:"regions" toon:"regions"`
}

// usrOrgResult holds one org's fetched XSUAA data (users and, for uar.csv,
// role collections) before it is projected into an output format.
type usrOrgResult struct {
	regionName      string
	orgGUID         string
	orgName         string
	users           []xsuaa.User
	roleCollections []xsuaa.RoleCollection
	err             error
}

// ── command ───────────────────────────────────────────────────────────────────

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "List XSUAA users across all accessible organizations",
	Long: `List users from the XSUAA (Authorization and Trust Management) apiaccess service
across one or more regions and organizations.

Output formats (--format):
  toon     Token-Oriented Object Notation — compact, human-readable (default)
  json     JSON document
  csv      CSV rows: region,org_id,org_name,user_id,user_externalId,user_origin,user_name,email,lastLogonTime,groups
  uar.csv  User Access Review CSV, one row per role collection membership.
           Columns: Role Collection,Description,Role Collection Members,Origin,Subaccount ID
           A user assigned to N role collections produces N rows, with the other
           fields duplicated across them. Role collections with no members still
           get one row, with "N/A" in Role Collection Members and Origin. Rows are
           sorted by Role Collection name. --fields/--excludeFields are ignored
           for this format since its columns are fixed.

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually (suppress with
--no-prompt to skip the org silently instead).

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a user matches if any user field
contains any of them (case-insensitive).

Use --output/-o to write the result to a file instead of stdout.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		outputFile, _ := cmd.Flags().GetString("output")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
		filter, _ := cmd.Flags().GetString("filter")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		fieldsCSV, _ := cmd.Flags().GetString("fields")
		excludeFieldsCSV, _ := cmd.Flags().GetString("excludeFields")
		fields := buildUsrFieldSet(fieldsCSV, excludeFieldsCSV)

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
		// --org takes precedence: scope resolveXsuaaClients to that single org so
		// other regions are not scanned unnecessarily.
		if orgGUID != "" {
			includeOrgs = cosOrgSet{cosOrgRef{ID: orgGUID}}
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

		// Phase 1: resolve XSUAA tokens for all accessible orgs.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
		if err != nil {
			return err
		}
		if orgGUID != "" && len(clients) == 0 {
			return fmt.Errorf("org %q not found in any accessible region", orgGUID)
		}

		isUARFormat := strings.ToLower(format) == "uar.csv"

		// Phase 2: fetch XSUAA users (and, for uar.csv, role collections) for
		// each org in parallel.
		results := make([]usrOrgResult, len(clients))
		var wg sync.WaitGroup

		for i, w := range clients {
			wg.Add(1)
			go func(idx int, w xsuaaOrgClient) {
				defer wg.Done()
				slog.Debug("fetching XSUAA users", "region", w.RegionName, "org", w.OrgName)
				users, err := xsuaa.ListUsers(ctx, w.APIURL, w.Token)
				if err != nil {
					results[idx] = usrOrgResult{regionName: w.RegionName, orgGUID: w.OrgGUID, orgName: w.OrgName, err: err}
					return
				}
				var rcs []xsuaa.RoleCollection
				if isUARFormat {
					rcs, err = xsuaa.ListRoleCollections(ctx, w.APIURL, w.Token)
					if err != nil {
						results[idx] = usrOrgResult{regionName: w.RegionName, orgGUID: w.OrgGUID, orgName: w.OrgName,
							err: fmt.Errorf("listing role collections: %w", err)}
						return
					}
				}
				results[idx] = usrOrgResult{
					regionName:      w.RegionName,
					orgGUID:         w.OrgGUID,
					orgName:         w.OrgName,
					users:           users,
					roleCollections: rcs,
				}
			}(i, w)
		}
		wg.Wait()

		// Phase 3: assemble output document, preserving region order.
		regionOrder := make([]string, 0)
		regionSeen := make(map[string]bool)
		for _, c := range clients {
			if !regionSeen[c.RegionName] {
				regionOrder = append(regionOrder, c.RegionName)
				regionSeen[c.RegionName] = true
			}
		}

		regionOrgs := make(map[string][]usrOutOrg)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.orgName, r.err)
				continue
			}
			var outUsers []usrOutUser
			for _, u := range r.users {
				email := xsuaa.PrimaryEmail(u.Emails)
				lastLogon := xsuaa.MSToISO(u.LastLogonTime)
				groups := xsuaa.GroupValues(u.Groups)
				if !usrMatchesFilter(u, email, lastLogon, groups, filter) {
					continue
				}
				if !usrMatchesIncludeExclude(u, email, lastLogon, groups, includePattern, excludePattern) {
					continue
				}
				outUsers = append(outUsers, usrApplyFields(u, email, lastLogon, groups, fields))
			}
			regionOrgs[r.regionName] = append(regionOrgs[r.regionName], usrOutOrg{
				ID:    r.orgGUID,
				Name:  r.orgName,
				Users: outUsers,
			})
		}

		var outRegions []usrOutRegion
		for _, rid := range regionOrder {
			orgs := regionOrgs[rid]
			if len(orgs) > 0 {
				outRegions = append(outRegions, usrOutRegion{ID: rid, Orgs: orgs})
			}
		}
		doc := usrOutDoc{Regions: outRegions}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			return writeUsersJSON(out, doc)
		case "csv":
			return writeUsersCSV(out, doc)
		case "uar.csv":
			return writeUsersUARCSV(out, buildUARRows(regionOrder, results, filter, includePattern, excludePattern))
		default: // "toon"
			return writeUsersToon(out, doc)
		}
	},
}

// uarMember is a single role-collection assignment: a user's email and origin.
type uarMember struct {
	Email  string
	Origin string
}

// uarRow is one output row of the uar.csv format.
type uarRow struct {
	RoleCollection string
	Description    string
	Member         string
	Origin         string
	SubaccountID   string
}

// buildUARRows pivots per-org users and role collections into one row per
// role collection membership (or one "N/A" row for role collections with no
// members), sorted by role collection name within each org.
func buildUARRows(regionOrder []string, results []usrOrgResult, filter, includePattern, excludePattern string) []uarRow {
	regionResults := make(map[string][]usrOrgResult)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		regionResults[r.regionName] = append(regionResults[r.regionName], r)
	}

	var rows []uarRow
	for _, rid := range regionOrder {
		for _, r := range regionResults[rid] {
			members := make(map[string][]uarMember)
			for _, u := range r.users {
				email := xsuaa.PrimaryEmail(u.Emails)
				lastLogon := xsuaa.MSToISO(u.LastLogonTime)
				groups := xsuaa.GroupValues(u.Groups)
				if !usrMatchesFilter(u, email, lastLogon, groups, filter) {
					continue
				}
				if !usrMatchesIncludeExclude(u, email, lastLogon, groups, includePattern, excludePattern) {
					continue
				}
				for _, g := range u.Groups {
					name := g.Display
					if name == "" {
						name = g.Value
					}
					members[name] = append(members[name], uarMember{Email: email, Origin: u.Origin})
				}
			}

			type rcEntry struct{ name, description string }
			seen := make(map[string]bool)
			var rcs []rcEntry
			for _, rc := range r.roleCollections {
				rcs = append(rcs, rcEntry{name: rc.Name, description: rc.Description})
				seen[rc.Name] = true
			}
			// Role collections found only via user membership (not returned by
			// ListRoleCollections) still get a row, with an empty description.
			for name := range members {
				if !seen[name] {
					rcs = append(rcs, rcEntry{name: name})
					seen[name] = true
				}
			}
			sort.Slice(rcs, func(i, j int) bool { return rcs[i].name < rcs[j].name })

			for _, rc := range rcs {
				mem := members[rc.name]
				if len(mem) == 0 {
					rows = append(rows, uarRow{
						RoleCollection: rc.name,
						Description:    rc.description,
						Member:         "N/A",
						Origin:         "N/A",
						SubaccountID:   r.orgGUID,
					})
					continue
				}
				sort.Slice(mem, func(i, j int) bool {
					if !strings.EqualFold(mem[i].Email, mem[j].Email) {
						return strings.ToLower(mem[i].Email) < strings.ToLower(mem[j].Email)
					}
					return mem[i].Origin < mem[j].Origin
				})
				for _, m := range mem {
					rows = append(rows, uarRow{
						RoleCollection: rc.name,
						Description:    rc.description,
						Member:         m.Email,
						Origin:         m.Origin,
						SubaccountID:   r.orgGUID,
					})
				}
			}
		}
	}
	return rows
}

// writeUsersUARCSV writes the uar.csv format: one row per role collection
// membership, columns Role Collection,Description,Role Collection Members,Origin,Subaccount ID.
func writeUsersUARCSV(w io.Writer, rows []uarRow) error {
	csvW := csv.NewWriter(w)
	defer csvW.Flush()

	if err := csvW.Write([]string{
		"Role Collection", "Description", "Role Collection Members", "Origin", "Subaccount ID",
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := csvW.Write([]string{
			row.RoleCollection, row.Description, row.Member, row.Origin, row.SubaccountID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func writeUsersToon(w io.Writer, doc usrOutDoc) error {
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	if _, err = w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func writeUsersJSON(w io.Writer, doc usrOutDoc) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

// writeUsersCSV writes one row per user with columns:
// region,org_id,org_name,user_id,user_externalId,user_origin,user_name,email,lastLogonTime,groups
func writeUsersCSV(w io.Writer, doc usrOutDoc) error {
	csvW := csv.NewWriter(w)
	defer csvW.Flush()

	if err := csvW.Write([]string{
		"region", "org_id", "org_name",
		"user_id", "user_externalId", "user_origin", "user_name", "email", "lastLogonTime", "groups",
	}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, u := range o.Users {
				if err := csvW.Write([]string{
					r.ID, o.ID, o.Name,
					u.ID, u.ExternalID, u.Origin, u.UserName, u.Email, u.LastLogonTime, u.Groups,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func init() {
	usersCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(usersCmd)
	usersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	usersCmd.Flags().String("format", "toon", "Output format: toon (default), json, csv, or uar.csv")
	usersCmd.Flags().String("org", "", "Org GUID to target; only users from this org will be fetched")
	usersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	usersCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	usersCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	usersCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
	usersCmd.Flags().String("filter", "", "Case-insensitive substring filter on any user field (user_id, user_externalId, user_origin, user_name, lastLogonTime, groups)")
	usersCmd.Flags().String("include", "", "Only include users where any user field contains any of these comma-separated, case-insensitive keywords")
	usersCmd.Flags().String("exclude", "", "Exclude users where any user field contains any of these comma-separated, case-insensitive keywords")
	usersCmd.Flags().String("fields", "", "Comma-separated fields to include in output (user_id,user_externalId,user_origin,user_name,email,lastLogonTime,groups)")
	usersCmd.Flags().String("excludeFields", "", "Comma-separated fields to exclude from output")
}

// usrFieldSet tracks which output fields are active. nil means all fields included.
type usrFieldSet map[string]bool

func (f usrFieldSet) active(field string) bool {
	return f == nil || f[field]
}

var usrAllFields = []string{"user_id", "user_externalId", "user_origin", "user_name", "email", "lastLogonTime", "groups"}

// buildUsrFieldSet computes the active field set from --fields and --excludeFields.
// Returns nil if both are empty (all fields active).
func buildUsrFieldSet(fieldsCSV, excludeCSV string) usrFieldSet {
	if fieldsCSV == "" && excludeCSV == "" {
		return nil
	}
	active := make(usrFieldSet)
	if fieldsCSV != "" {
		for _, f := range splitCSV(fieldsCSV) {
			active[strings.TrimSpace(f)] = true
		}
	} else {
		for _, f := range usrAllFields {
			active[f] = true
		}
	}
	for _, f := range splitCSV(excludeCSV) {
		delete(active, strings.TrimSpace(f))
	}
	return active
}

// usrMatchesFilter reports whether a user matches the given substring filter.
// Empty filter matches all users.
func usrMatchesFilter(u xsuaa.User, email, lastLogon, groups, filter string) bool {
	if filter == "" {
		return true
	}
	fl := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(u.ID), fl) ||
		strings.Contains(strings.ToLower(u.ExternalID), fl) ||
		strings.Contains(strings.ToLower(u.Origin), fl) ||
		strings.Contains(strings.ToLower(u.UserName), fl) ||
		strings.Contains(strings.ToLower(email), fl) ||
		strings.Contains(strings.ToLower(lastLogon), fl) ||
		strings.Contains(strings.ToLower(groups), fl)
}

// usrMatchesIncludeExclude applies --include/--exclude keyword filtering
// (comma-separated, case-insensitive, matched if any keyword is a substring
// of any field) against the same fields usrMatchesFilter checks.
func usrMatchesIncludeExclude(u xsuaa.User, email, lastLogon, groups, includePattern, excludePattern string) bool {
	fields := []string{u.ID, u.ExternalID, u.Origin, u.UserName, email, lastLogon, groups}
	if includePattern != "" && !skipMatches(includePattern, fields...) {
		return false
	}
	if excludePattern != "" && skipMatches(excludePattern, fields...) {
		return false
	}
	return true
}

// usrApplyFields builds a usrOutUser, omitting fields not in the active set.
func usrApplyFields(u xsuaa.User, email, lastLogon, groups string, fields usrFieldSet) usrOutUser {
	var out usrOutUser
	if fields.active("user_id") {
		out.ID = u.ID
	}
	if fields.active("user_externalId") {
		out.ExternalID = u.ExternalID
	}
	if fields.active("user_origin") {
		out.Origin = u.Origin
	}
	if fields.active("user_name") {
		out.UserName = u.UserName
	}
	if fields.active("email") {
		out.Email = email
	}
	if fields.active("lastLogonTime") {
		out.LastLogonTime = lastLogon
	}
	if fields.active("groups") {
		out.Groups = groups
	}
	return out
}
