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
	ID            string `toon:"user_id"`
	ExternalID    string `toon:"user_externalId"`
	Origin        string `toon:"user_origin"`
	UserName      string `toon:"userName"`
	Email         string `toon:"email"`
	LastLogonTime string `toon:"lastLogonTime"`
	Groups        string `toon:"groups"`
}

type usrOutOrg struct {
	ID    string       `toon:"org_id"`
	Name  string       `toon:"org_name"`
	Users []usrOutUser `toon:"users"`
}

type usrOutRegion struct {
	ID   string      `toon:"region"`
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

For each org the command finds any xsuaa/apiaccess service instance (in any space)
and uses the first available service key to obtain an access token. If no instance
or key exists, a prompt offers instructions to create them manually (suppress with
--no-prompt to skip the org silently instead).

Only the access token is cached in ~/.bo/credentials.json — service key credentials
are fetched from CF on demand and never stored locally.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
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

		// Phase 1: resolve XSUAA tokens for all accessible orgs.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
		if err != nil {
			return err
		}
		if orgGUID != "" && len(clients) == 0 {
			return fmt.Errorf("org %q not found in any accessible region", orgGUID)
		}

		// Phase 2: fetch XSUAA users for each org in parallel.
		type orgResult struct {
			regionName string
			orgGUID    string
			orgName    string
			users      []xsuaa.User
			err        error
		}
		results := make([]orgResult, len(clients))
		var wg sync.WaitGroup

		for i, w := range clients {
			wg.Add(1)
			go func(idx int, w xsuaaOrgClient) {
				defer wg.Done()
				slog.Debug("fetching XSUAA users", "region", w.RegionName, "org", w.OrgName)
				users, err := xsuaa.ListUsers(ctx, w.APIURL, w.Token)
				results[idx] = orgResult{
					regionName: w.RegionName,
					orgGUID:    w.OrgGUID,
					orgName:    w.OrgName,
					users:      users,
					err:        err,
				}
			}(i, w)
		}
		wg.Wait()

		// Phase 3: assemble and print TOON output, preserving region order.
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
	usersCmd.Flags().String("org", "", "Org GUID to target; only users from this org will be fetched")
	usersCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	usersCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	usersCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
	usersCmd.Flags().String("filter", "", "Case-insensitive substring filter on any user field (user_id, user_externalId, user_origin, userName, lastLogonTime, groups)")
	usersCmd.Flags().String("fields", "", "Comma-separated fields to include in output (user_id,user_externalId,user_origin,userName,email,lastLogonTime,groups)")
	usersCmd.Flags().String("excludeFields", "", "Comma-separated fields to exclude from output")
}

// usrFieldSet tracks which output fields are active. nil means all fields included.
type usrFieldSet map[string]bool

func (f usrFieldSet) active(field string) bool {
	return f == nil || f[field]
}

var usrAllFields = []string{"user_id", "user_externalId", "user_origin", "userName", "email", "lastLogonTime", "groups"}

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
	if fields.active("userName") {
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
