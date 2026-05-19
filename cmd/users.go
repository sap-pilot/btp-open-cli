package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// ── output types ─────────────────────────────────────────────────────────────

type usrOutUser struct {
	ID            string `toon:"id"`
	ExternalID    string `toon:"externalId"`
	Origin        string `toon:"origin"`
	UserName      string `toon:"userName"`
	LastLogonTime string `toon:"lastLogonTime"`
	Groups        string `toon:"groups"`
}

type usrOutOrg struct {
	ID    string       `toon:"id"`
	Name  string       `toon:"name"`
	Users []usrOutUser `toon:"users"`
}

type usrOutRegion struct {
	ID   string      `toon:"id"`
	Orgs []usrOutOrg `toon:"orgs"`
}

type usrOutDoc struct {
	Regions []usrOutRegion `toon:"regions"`
}

// ── command ───────────────────────────────────────────────────────────────────

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "List XSUAA users across all accessible organizations",
	Long: `List users from the XSUAA (Authorization and Trust Management) apiaccess service
across one or more regions and organizations.

For each org the command ensures the service instance 'btp-xsuaa' (xsuaa/apiaccess)
and service key 'btp-open-cli-sk' exist in the 'util' space. If they do not exist,
a TOON preview is shown and confirmation is required before creating them.

Credentials are cached in ~/.bo/credentials.json and reused across runs.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		filter, _ := cmd.Flags().GetString("filter")
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

		var excludeOrgs cosOrgSet
		if excludeOrgsFile != "" {
			excludeOrgs, err = parseCosOrgCSV(excludeOrgsFile)
			if err != nil {
				return fmt.Errorf("invalid --excludeOrgs CSV: %w", err)
			}
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Phase 1: discover orgs and check xsuaa service/key status.
		plans := discoverXsuaaPlans(ctx, apiURLs, creds, includeOrgs, excludeOrgs)

		// Phase 2+3: preview, confirm, create instances/keys, cache credentials.
		creds, proceed, err := ensureXsuaaCredentials(ctx, plans, creds, skipConfirm)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}

		// Phase 4: reload credentials, then fetch XSUAA users for each org in parallel.
		creds, err = store.Load()
		if err != nil {
			return fmt.Errorf("loading credentials: %w", err)
		}

		type orgWorkItem struct {
			regionName string
			orgGUID    string
			orgName    string
		}
		var work []orgWorkItem
		for _, plan := range plans {
			if plan.APIURL == "" {
				continue
			}
			for _, op := range plan.Orgs {
				xd, ok := creds.OrgXsuaa[op.Org.GUID]
				if !ok || xd.ClientID == "" || xd.ClientSecret == "" || xd.URL == "" {
					fmt.Fprintf(os.Stderr, "[%s] %s: no XSUAA credentials — skipping\n",
						plan.Region, op.Org.Name)
					continue
				}
				work = append(work, orgWorkItem{
					regionName: plan.Region,
					orgGUID:    op.Org.GUID,
					orgName:    op.Org.Name,
				})
			}
		}

		type orgResult struct {
			regionName string
			orgGUID    string
			orgName    string
			users      []xsuaa.User
			err        error
		}
		results := make([]orgResult, len(work))
		var wg sync.WaitGroup
		var credsMu sync.Mutex

		for i, w := range work {
			wg.Add(1)
			go func(idx int, w orgWorkItem) {
				defer wg.Done()

				xd, err := xsuaaRefreshToken(ctx, w.orgGUID, creds, &credsMu)
				if err != nil {
					results[idx] = orgResult{regionName: w.regionName, orgGUID: w.orgGUID, orgName: w.orgName, err: err}
					return
				}
				slog.Debug("fetching XSUAA users", "region", w.regionName, "org", w.orgName)

				apiBaseURL := xsuaa.APIBaseURL(w.regionName)
				users, err := xsuaa.ListUsers(ctx, apiBaseURL, xd.AccessToken)
				if err != nil {
					results[idx] = orgResult{regionName: w.regionName, orgGUID: w.orgGUID, orgName: w.orgName, err: err}
					return
				}
				results[idx] = orgResult{regionName: w.regionName, orgGUID: w.orgGUID, orgName: w.orgName, users: users}
			}(i, w)
		}
		wg.Wait()

		// Phase 5: assemble and print TOON output, preserving region order.
		regionOrder := make([]string, 0, len(plans))
		regionSeen := make(map[string]bool)
		for _, plan := range plans {
			if plan.APIURL != "" && !regionSeen[plan.Region] {
				regionOrder = append(regionOrder, plan.Region)
				regionSeen[plan.Region] = true
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
				lastLogon := xsuaa.MSToISO(u.LastLogonTime)
				groups := xsuaa.GroupValues(u.Groups)
				if !usrMatchesFilter(u, lastLogon, groups, filter) {
					continue
				}
				outUsers = append(outUsers, usrApplyFields(u, lastLogon, groups, fields))
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

		out, err := toonenc.Marshal(usrOutDoc{Regions: outRegions}, toonenc.WithIndent(2))
		if err != nil {
			return fmt.Errorf("encoding output: %w", err)
		}
		if _, err = os.Stdout.Write(out); err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout)
		return err
	},
}

func init() {
	rootCmd.AddCommand(usersCmd)
	usersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	usersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,id,name)")
	usersCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,id,name)")
	usersCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt for service/key creation")
	usersCmd.Flags().String("filter", "", "Case-insensitive substring filter on any user field (id, externalId, origin, userName, lastLogonTime, groups)")
	usersCmd.Flags().String("fields", "", "Comma-separated fields to include in output (id,externalId,origin,userName,lastLogonTime,groups)")
	usersCmd.Flags().String("excludeFields", "", "Comma-separated fields to exclude from output")
}

// usrFieldSet tracks which output fields are active. nil means all fields included.
type usrFieldSet map[string]bool

func (f usrFieldSet) active(field string) bool {
	return f == nil || f[field]
}

var usrAllFields = []string{"id", "externalId", "origin", "userName", "lastLogonTime", "groups"}

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
func usrMatchesFilter(u xsuaa.User, lastLogon, groups, filter string) bool {
	if filter == "" {
		return true
	}
	fl := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(u.ID), fl) ||
		strings.Contains(strings.ToLower(u.ExternalID), fl) ||
		strings.Contains(strings.ToLower(u.Origin), fl) ||
		strings.Contains(strings.ToLower(u.UserName), fl) ||
		strings.Contains(strings.ToLower(lastLogon), fl) ||
		strings.Contains(strings.ToLower(groups), fl)
}

// usrApplyFields builds a usrOutUser, omitting fields not in the active set.
func usrApplyFields(u xsuaa.User, lastLogon, groups string, fields usrFieldSet) usrOutUser {
	var out usrOutUser
	if fields.active("id") {
		out.ID = u.ID
	}
	if fields.active("externalId") {
		out.ExternalID = u.ExternalID
	}
	if fields.active("origin") {
		out.Origin = u.Origin
	}
	if fields.active("userName") {
		out.UserName = u.UserName
	}
	if fields.active("lastLogonTime") {
		out.LastLogonTime = lastLogon
	}
	if fields.active("groups") {
		out.Groups = groups
	}
	return out
}
