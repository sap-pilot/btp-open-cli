package cmd

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

const (
	usrServiceOffering = "xsuaa"
	usrServicePlan     = "apiaccess"
	usrInstanceName    = "btp-xsuaa"
	usrKeyName         = "btp-open-cli-sk"
	usrUtilSpace       = "util"
)

// ── discovery plan types ──────────────────────────────────────────────────────

type usrOrgPlan struct {
	Org           cf.Organization
	UtilSpaceGUID string
	UtilSpaceName string
	NeedsInstance bool // service instance must be created
	NeedsKey      bool // service key must be created (may need instance first)
	NeedsFetch    bool // instance+key exist but credentials not cached
	InstanceGUID  string
	KeyGUID       string
	XsuaaReady    bool // credentials already in store — skip CF checks
}

type usrRegionPlan struct {
	Region      string
	APIURL      string
	ServicePlan *cf.ServicePlan // apiaccess plan; needed only for NeedsInstance orgs
	Orgs        []usrOrgPlan
}

// ── setup preview types ───────────────────────────────────────────────────────

type usrSetupSpace struct {
	ID   string `toon:"id"`
	Name string `toon:"name"`
}

type usrSetupOrg struct {
	ID     string          `toon:"id"`
	Name   string          `toon:"name"`
	Spaces []usrSetupSpace `toon:"spaces"`
}

type usrSetupRegion struct {
	ID   string        `toon:"id"`
	Orgs []usrSetupOrg `toon:"orgs"`
}

type usrSetupDoc struct {
	Regions []usrSetupRegion `toon:"regions"`
}

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

		// Phase 1: discover orgs and check xsuaa service/key status in parallel.
		plans := make([]usrRegionPlan, len(apiURLs))
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] no token — run: bo login --regions %s\n", regionName, regionName)
					mu.Unlock()
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, err)
					mu.Unlock()
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				var orgPlans []usrOrgPlan
				var needsInstanceCreate bool

				for _, org := range orgs {
					if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}

					plan := usrOrgPlan{Org: org}

					// Check if credentials already cached.
					mu.Lock()
					xd, hasXsuaa := creds.OrgXsuaa[org.GUID]
					mu.Unlock()

					if hasXsuaa && xd.ClientID != "" && xd.ClientSecret != "" && xd.URL != "" {
						plan.XsuaaReady = true
						orgPlans = append(orgPlans, plan)
						continue
					}

					// Find the util space.
					spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: error listing spaces: %v\n", regionName, org.Name, err)
						mu.Unlock()
						continue
					}
					var utilSpace *cf.Space
					for i := range spaces {
						if strings.EqualFold(spaces[i].Name, usrUtilSpace) {
							utilSpace = &spaces[i]
							break
						}
					}
					if utilSpace == nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: no '%s' space found — skipping\n", regionName, org.Name, usrUtilSpace)
						mu.Unlock()
						continue
					}
					plan.UtilSpaceGUID = utilSpace.GUID
					plan.UtilSpaceName = utilSpace.Name

					// Check service instance.
					inst, err := client.FindServiceInstance(ctx, usrInstanceName, utilSpace.GUID)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] %s: error checking service instance: %v\n", regionName, org.Name, err)
						mu.Unlock()
						continue
					}

					if inst == nil {
						plan.NeedsInstance = true
						plan.NeedsKey = true
						needsInstanceCreate = true
					} else {
						plan.InstanceGUID = inst.GUID

						key, err := client.FindServiceCredentialBinding(ctx, usrKeyName, inst.GUID)
						if err != nil {
							mu.Lock()
							fmt.Fprintf(os.Stderr, "[%s] %s: error checking service key: %v\n", regionName, org.Name, err)
							mu.Unlock()
							continue
						}
						if key == nil {
							plan.NeedsKey = true
						} else {
							plan.KeyGUID = key.GUID
							plan.NeedsFetch = true // key exists; just cache credentials
						}
					}

					orgPlans = append(orgPlans, plan)
				}

				// Look up the service plan only if any org needs an instance created.
				var servicePlan *cf.ServicePlan
				if needsInstanceCreate {
					sp, err := client.FindServicePlan(ctx, usrServiceOffering, usrServicePlan)
					if err != nil {
						mu.Lock()
						fmt.Fprintf(os.Stderr, "[%s] error looking up service plan %s/%s: %v\n",
							regionName, usrServiceOffering, usrServicePlan, err)
						mu.Unlock()
					} else {
						servicePlan = sp
					}
				}

				plans[idx] = usrRegionPlan{
					Region:      regionName,
					APIURL:      url,
					ServicePlan: servicePlan,
					Orgs:        orgPlans,
				}
			}(i, apiURL)
		}
		wg.Wait()

		// Phase 2: if any service instance or key needs to be created, preview and confirm.
		var setupNeeded bool
		for _, plan := range plans {
			for _, op := range plan.Orgs {
				if op.NeedsInstance || op.NeedsKey {
					setupNeeded = true
					break
				}
			}
			if setupNeeded {
				break
			}
		}

		if setupNeeded && !skipConfirm {
			if err := usrPrintSetupPreview(plans); err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Proceed with service/key creation? [y/N] ")
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
			fmt.Fprintln(os.Stdout)
		}

		// Phase 3: create missing service instances/keys and cache credentials.
		// Also handles NeedsFetch (key already exists; just read and cache credentials).
		anyPhase3 := setupNeeded
		if !anyPhase3 {
			for _, plan := range plans {
				for _, op := range plan.Orgs {
					if op.NeedsFetch {
						anyPhase3 = true
						break
					}
				}
				if anyPhase3 {
					break
				}
			}
		}

		if anyPhase3 {
			creds, err = store.Load()
			if err != nil {
				return fmt.Errorf("loading credentials: %w", err)
			}
			if creds.OrgXsuaa == nil {
				creds.OrgXsuaa = make(map[string]store.XsuaaData)
			}

			for ri := range plans {
				plan := &plans[ri]
				if plan.APIURL == "" {
					continue
				}
				tok, ok := creds.Tokens[plan.APIURL]
				if !ok {
					continue
				}
				client := cf.NewClient(plan.APIURL, tok.AccessToken)

				for oi := range plan.Orgs {
					op := &plan.Orgs[oi]
					if op.XsuaaReady || (!op.NeedsInstance && !op.NeedsKey && !op.NeedsFetch) {
						continue
					}

					if op.NeedsInstance {
						if plan.ServicePlan == nil {
							fmt.Fprintf(os.Stderr, "[%s] %s: service plan %s/%s not found — skipping\n",
								plan.Region, op.Org.Name, usrServiceOffering, usrServicePlan)
							continue
						}
						fmt.Fprintf(os.Stdout, "[%s] %s: creating service instance '%s'...\n",
							plan.Region, op.Org.Name, usrInstanceName)
						if err := client.CreateServiceInstance(ctx, usrInstanceName, op.UtilSpaceGUID, plan.ServicePlan.GUID); err != nil {
							fmt.Fprintf(os.Stderr, "[%s] %s: failed to create service instance: %v\n",
								plan.Region, op.Org.Name, err)
							continue
						}

						fmt.Fprintln(os.Stdout, "Waiting 8 s for CF async processing...")
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(8 * time.Second):
						}

						inst, err := client.FindServiceInstance(ctx, usrInstanceName, op.UtilSpaceGUID)
						if err != nil || inst == nil {
							fmt.Fprintf(os.Stderr, "[%s] %s: could not find newly created service instance: %v\n",
								plan.Region, op.Org.Name, err)
							continue
						}
						op.InstanceGUID = inst.GUID
					}

					if op.NeedsKey {
						fmt.Fprintf(os.Stdout, "[%s] %s: creating service key '%s'...\n",
							plan.Region, op.Org.Name, usrKeyName)
						if err := client.CreateServiceCredentialBinding(ctx, usrKeyName, op.InstanceGUID); err != nil {
							fmt.Fprintf(os.Stderr, "[%s] %s: failed to create service key: %v\n",
								plan.Region, op.Org.Name, err)
							continue
						}

						fmt.Fprintln(os.Stdout, "Waiting 8 s for CF async processing...")
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(8 * time.Second):
						}

						key, err := client.FindServiceCredentialBinding(ctx, usrKeyName, op.InstanceGUID)
						if err != nil || key == nil {
							fmt.Fprintf(os.Stderr, "[%s] %s: could not find newly created service key: %v\n",
								plan.Region, op.Org.Name, err)
							continue
						}
						op.KeyGUID = key.GUID
					}

					// Fetch and cache credentials (covers NeedsKey=true and NeedsFetch=true).
					details, err := client.GetServiceCredentialDetails(ctx, op.KeyGUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[%s] %s: failed to fetch service key credentials: %v\n",
							plan.Region, op.Org.Name, err)
						continue
					}

					clientID, _ := details["clientid"].(string)
					clientSecret, _ := details["clientsecret"].(string)
					xsuaaURL, _ := details["url"].(string)
					if clientID == "" || clientSecret == "" || xsuaaURL == "" {
						fmt.Fprintf(os.Stderr, "[%s] %s: incomplete credentials in service key\n",
							plan.Region, op.Org.Name)
						continue
					}

					creds.OrgXsuaa[op.Org.GUID] = store.XsuaaData{
						ClientID:     clientID,
						ClientSecret: clientSecret,
						URL:          xsuaaURL,
					}
					if err := store.Save(creds); err != nil {
						fmt.Fprintf(os.Stderr, "[%s] %s: failed to save credentials: %v\n",
							plan.Region, op.Org.Name, err)
					} else {
						slog.Debug("XSUAA credentials saved", "region", plan.Region, "org", op.Org.Name)
					}
				}
			}
		}

		// Phase 4: reload credentials, then fetch XSUAA users for each org in parallel.
		creds, err = store.Load()
		if err != nil {
			return fmt.Errorf("loading credentials: %w", err)
		}

		type orgWork struct {
			regionName string
			org        cf.Organization
		}
		var work []orgWork
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
				work = append(work, orgWork{regionName: plan.Region, org: op.Org})
			}
		}

		type orgResult struct {
			regionName string
			org        cf.Organization
			users      []xsuaa.User
			err        error
		}
		results := make([]orgResult, len(work))
		var wg2 sync.WaitGroup
		var credsMu sync.Mutex

		for i, w := range work {
			wg2.Add(1)
			go func(idx int, w orgWork) {
				defer wg2.Done()

				credsMu.Lock()
				xd := creds.OrgXsuaa[w.org.GUID]
				credsMu.Unlock()

				// Refresh token if absent or within 60 s of expiry.
				if xd.AccessToken == "" || time.Now().Add(60*time.Second).After(xd.TokenExpiry) {
					slog.Debug("refreshing XSUAA token", "region", w.regionName, "org", w.org.Name)
					token, expiry, err := xsuaa.GetAccessToken(ctx, xd.URL, xd.ClientID, xd.ClientSecret)
					if err != nil {
						results[idx] = orgResult{regionName: w.regionName, org: w.org,
							err: fmt.Errorf("XSUAA token: %w", err)}
						return
					}
					xd.AccessToken = token
					xd.TokenExpiry = expiry

					credsMu.Lock()
					creds.OrgXsuaa[w.org.GUID] = xd
					_ = store.Save(creds)
					credsMu.Unlock()
				}

				apiBaseURL := xsuaa.APIBaseURL(w.regionName)
				users, err := xsuaa.ListUsers(ctx, apiBaseURL, xd.AccessToken)
				if err != nil {
					results[idx] = orgResult{regionName: w.regionName, org: w.org, err: err}
					return
				}
				results[idx] = orgResult{regionName: w.regionName, org: w.org, users: users}
			}(i, w)
		}
		wg2.Wait()

		// Phase 5: assemble and print TOON output.
		// Preserve region order from plans.
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
				fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.regionName, r.org.Name, r.err)
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
				ID:    r.org.GUID,
				Name:  r.org.Name,
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

// usrPrintSetupPreview prints a TOON preview of the util spaces where the
// service instance or key will be created.
func usrPrintSetupPreview(plans []usrRegionPlan) error {
	var previewRegions []usrSetupRegion
	for _, plan := range plans {
		pr := usrSetupRegion{ID: plan.Region}
		for _, op := range plan.Orgs {
			if !op.NeedsInstance && !op.NeedsKey {
				continue
			}
			po := usrSetupOrg{
				ID:   op.Org.GUID,
				Name: op.Org.Name,
				Spaces: []usrSetupSpace{
					{ID: op.UtilSpaceGUID, Name: op.UtilSpaceName},
				},
			}
			pr.Orgs = append(pr.Orgs, po)
		}
		if len(pr.Orgs) > 0 {
			previewRegions = append(previewRegions, pr)
		}
	}

	out, err := toonenc.Marshal(usrSetupDoc{Regions: previewRegions}, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding setup preview: %w", err)
	}
	fmt.Fprintln(os.Stdout, "The following service instance/key will be created in the 'util' space:")
	os.Stdout.Write(out)
	fmt.Fprintln(os.Stdout)
	return nil
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
// If both are empty, nil is returned (all fields active).
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

// usrMatchesFilter reports whether a user matches the given substring filter
// (case-insensitive, checked against all fields). Empty filter matches all users.
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
