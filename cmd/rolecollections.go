package cmd

import (
	"encoding/json"
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

type rcOutRole struct {
	RoleTemplateAppID string `json:"roleTemplateAppId" toon:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"  toon:"roleTemplateName"`
	Name              string `json:"name"              toon:"name"`
	AppName           string `json:"appName"           toon:"appName"`
	Description       string `json:"description"       toon:"description"`
	IsReadOnly        bool   `json:"isReadOnly"        toon:"isReadOnly"`
}

type rcOutRoleRef struct {
	RoleTemplateAppID string `json:"roleTemplateAppId" toon:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"  toon:"roleTemplateName"`
	Name              string `json:"name"              toon:"name"`
	Description       string `json:"description"       toon:"description"`
}

type rcOutRoleCollection struct {
	Name           string         `json:"name"           toon:"name"`
	Description    string         `json:"description"    toon:"description"`
	IsReadOnly     bool           `json:"isReadOnly"     toon:"isReadOnly"`
	RoleReferences []rcOutRoleRef `json:"roleReferences" toon:"roleReferences"`
}

type rcOutOrg struct {
	ID              string                `json:"id"              toon:"id"`
	Name            string                `json:"name"            toon:"name"`
	Roles           []rcOutRole           `json:"roles"           toon:"roles"`
	RoleCollections []rcOutRoleCollection `json:"roleCollections" toon:"roleCollections"`
}

type rcOutRegion struct {
	ID   string     `json:"id"   toon:"id"`
	Orgs []rcOutOrg `json:"orgs" toon:"orgs"`
}

type rcOutDoc struct {
	Regions []rcOutRegion `json:"regions" toon:"regions"`
}

// ── command ───────────────────────────────────────────────────────────────────

var roleCollectionsCmd = &cobra.Command{
	Use:   "role-collections",
	Short: "List XSUAA roles and role collections across all accessible organizations",
	Long: `List roles and role collections from the XSUAA Authorization API across one
or more regions and organizations.

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
		format, _ := cmd.Flags().GetString("format")

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

		// Phase 4: reload credentials, then fetch roles + role collections per org in parallel.
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
			regionName      string
			orgGUID         string
			orgName         string
			roles           []xsuaa.Role
			roleCollections []xsuaa.RoleCollection
			err             error
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
				slog.Debug("fetching XSUAA roles", "region", w.regionName, "org", w.orgName)

				apiBaseURL := xsuaa.APIBaseURL(w.regionName)

				roles, err := xsuaa.ListRoles(ctx, apiBaseURL, xd.AccessToken)
				if err != nil {
					results[idx] = orgResult{regionName: w.regionName, orgGUID: w.orgGUID, orgName: w.orgName,
						err: fmt.Errorf("listing roles: %w", err)}
					return
				}

				rcs, err := xsuaa.ListRoleCollections(ctx, apiBaseURL, xd.AccessToken)
				if err != nil {
					results[idx] = orgResult{regionName: w.regionName, orgGUID: w.orgGUID, orgName: w.orgName,
						err: fmt.Errorf("listing role collections: %w", err)}
					return
				}

				results[idx] = orgResult{
					regionName:      w.regionName,
					orgGUID:         w.orgGUID,
					orgName:         w.orgName,
					roles:           roles,
					roleCollections: rcs,
				}
			}(i, w)
		}
		wg.Wait()

		// Phase 5: assemble output, preserving region order from plans.
		regionOrder := make([]string, 0, len(plans))
		regionSeen := make(map[string]bool)
		for _, plan := range plans {
			if plan.APIURL != "" && !regionSeen[plan.Region] {
				regionOrder = append(regionOrder, plan.Region)
				regionSeen[plan.Region] = true
			}
		}

		regionOrgs := make(map[string][]rcOutOrg)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.orgName, r.err)
				continue
			}

			var outRoles []rcOutRole
			for _, role := range r.roles {
				outRoles = append(outRoles, rcOutRole{
					RoleTemplateAppID: role.RoleTemplateAppID,
					RoleTemplateName:  role.RoleTemplateName,
					Name:              role.Name,
					AppName:           role.AppName,
					Description:       role.Description,
					IsReadOnly:        role.IsReadOnly,
				})
			}

			var outRCs []rcOutRoleCollection
			for _, rc := range r.roleCollections {
				var refs []rcOutRoleRef
				for _, ref := range rc.RoleReferences {
					refs = append(refs, rcOutRoleRef{
						RoleTemplateAppID: ref.RoleTemplateAppID,
						RoleTemplateName:  ref.RoleTemplateName,
						Name:              ref.Name,
						Description:       ref.Description,
					})
				}
				outRCs = append(outRCs, rcOutRoleCollection{
					Name:           rc.Name,
					Description:    rc.Description,
					IsReadOnly:     rc.IsReadOnly,
					RoleReferences: refs,
				})
			}

			regionOrgs[r.regionName] = append(regionOrgs[r.regionName], rcOutOrg{
				ID:              r.orgGUID,
				Name:            r.orgName,
				Roles:           outRoles,
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

		switch strings.ToLower(format) {
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
	rootCmd.AddCommand(roleCollectionsCmd)
	roleCollectionsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	roleCollectionsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,id,name)")
	roleCollectionsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,id,name)")
	roleCollectionsCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt for service/key creation")
	roleCollectionsCmd.Flags().String("format", "toon", "Output format: toon (default) or json")
}
