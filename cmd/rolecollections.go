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

type rcOutRole struct {
	RoleTemplateAppID string `json:"roleTemplateAppId" toon:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"  toon:"roleTemplateName"`
	Name              string `json:"role_name"         toon:"role_name"`
	AppName           string `json:"appName"           toon:"appName"`
	Description       string `json:"description"       toon:"description"`
	IsReadOnly        bool   `json:"isReadOnly"        toon:"isReadOnly"`
}

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
	Roles           []rcOutRole           `json:"roles"           toon:"roles"`
	RoleCollections []rcOutRoleCollection `json:"roleCollections" toon:"roleCollections"`
}

type rcOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []rcOutOrg `json:"orgs"   toon:"orgs"`
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
		orgFilter, _ := cmd.Flags().GetString("org")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
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
		}

		// Phase 1: resolve XSUAA tokens for the target orgs.
		clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
		if err != nil {
			return err
		}

		// Phase 2: fetch roles and role collections per org in parallel.
		type orgResult struct {
			regionName      string
			orgGUID         string
			orgName         string
			roles           []xsuaa.Role
			roleCollections []xsuaa.RoleCollection
			err             error
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
					roles:           roles,
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
			sort.Slice(outRoles, func(i, j int) bool {
				if outRoles[i].RoleTemplateAppID != outRoles[j].RoleTemplateAppID {
					return outRoles[i].RoleTemplateAppID < outRoles[j].RoleTemplateAppID
				}
				return outRoles[i].RoleTemplateName < outRoles[j].RoleTemplateName
			})

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
				sort.Slice(refs, func(i, j int) bool {
					if refs[i].RoleTemplateAppID != refs[j].RoleTemplateAppID {
						return refs[i].RoleTemplateAppID < refs[j].RoleTemplateAppID
					}
					return refs[i].RoleTemplateName < refs[j].RoleTemplateName
				})
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
	roleCollectionsCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(roleCollectionsCmd)
	roleCollectionsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	roleCollectionsCmd.Flags().String("org", "", "Org name or GUID to target (case-insensitive substring match on name, exact on GUID)")
	roleCollectionsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	roleCollectionsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	roleCollectionsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped")
	roleCollectionsCmd.Flags().String("format", "toon", "Output format: toon (default) or json")
}
