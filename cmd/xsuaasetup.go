package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"
)

const (
	xsuaaServiceOffering = "xsuaa"
	xsuaaServicePlan     = "apiaccess"
	xsuaaInstanceName    = "btp-xsuaa"
	xsuaaKeyName         = "btp-open-cli-sk"
	xsuaaUtilSpace       = "util"
)

// ── plan types ────────────────────────────────────────────────────────────────

type xsuaaOrgPlan struct {
	Org           cf.Organization
	UtilSpaceGUID string
	UtilSpaceName string
	NeedsInstance bool
	NeedsKey      bool
	NeedsFetch    bool
	InstanceGUID  string
	KeyGUID       string
	XsuaaReady    bool
}

type xsuaaRegionPlan struct {
	Region      string
	APIURL      string
	ServicePlan *cf.ServicePlan
	Orgs        []xsuaaOrgPlan
}

// ── setup preview types ───────────────────────────────────────────────────────

type xsuaaSetupSpace struct {
	ID   string `toon:"id"`
	Name string `toon:"name"`
}

type xsuaaSetupOrg struct {
	ID     string            `toon:"id"`
	Name   string            `toon:"name"`
	Spaces []xsuaaSetupSpace `toon:"spaces"`
}

type xsuaaSetupRegion struct {
	ID   string          `toon:"id"`
	Orgs []xsuaaSetupOrg `toon:"orgs"`
}

type xsuaaSetupDoc struct {
	Regions []xsuaaSetupRegion `toon:"regions"`
}

// discoverXsuaaPlans queries CF in parallel to determine which orgs need
// service instance/key creation and which already have cached credentials.
func discoverXsuaaPlans(ctx context.Context, apiURLs []string, creds *store.Credentials, includeOrgs, excludeOrgs cosOrgSet) []xsuaaRegionPlan {
	plans := make([]xsuaaRegionPlan, len(apiURLs))
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
			client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				mu.Lock()
				fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, err)
				mu.Unlock()
				return
			}
			slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

			var orgPlans []xsuaaOrgPlan
			var needsInstanceCreate bool

			for _, org := range orgs {
				if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
					continue
				}
				if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
					continue
				}

				plan := xsuaaOrgPlan{Org: org}

				mu.Lock()
				xd, hasXsuaa := creds.OrgXsuaa[org.GUID]
				mu.Unlock()

				if hasXsuaa && xd.ClientID != "" && xd.ClientSecret != "" && xd.URL != "" {
					plan.XsuaaReady = true
					orgPlans = append(orgPlans, plan)
					continue
				}

				spaces, err := client.ListOrganizationSpaces(ctx, org.GUID)
				if err != nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] %s: error listing spaces: %v\n", regionName, org.Name, err)
					mu.Unlock()
					continue
				}
				var utilSpace *cf.Space
				for i := range spaces {
					if strings.EqualFold(spaces[i].Name, xsuaaUtilSpace) {
						utilSpace = &spaces[i]
						break
					}
				}
				if utilSpace == nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] %s: no '%s' space found — skipping\n", regionName, org.Name, xsuaaUtilSpace)
					mu.Unlock()
					continue
				}
				plan.UtilSpaceGUID = utilSpace.GUID
				plan.UtilSpaceName = utilSpace.Name

				inst, err := client.FindServiceInstance(ctx, xsuaaInstanceName, utilSpace.GUID)
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

					key, err := client.FindServiceCredentialBinding(ctx, xsuaaKeyName, inst.GUID)
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
						plan.NeedsFetch = true
					}
				}

				orgPlans = append(orgPlans, plan)
			}

			var servicePlan *cf.ServicePlan
			if needsInstanceCreate {
				sp, err := client.FindServicePlan(ctx, xsuaaServiceOffering, xsuaaServicePlan)
				if err != nil {
					mu.Lock()
					fmt.Fprintf(os.Stderr, "[%s] error looking up service plan %s/%s: %v\n",
						regionName, xsuaaServiceOffering, xsuaaServicePlan, err)
					mu.Unlock()
				} else {
					servicePlan = sp
				}
			}

			plans[idx] = xsuaaRegionPlan{
				Region:      regionName,
				APIURL:      url,
				ServicePlan: servicePlan,
				Orgs:        orgPlans,
			}
		}(i, apiURL)
	}
	wg.Wait()
	return plans
}

// ensureXsuaaCredentials shows a preview, prompts for confirmation, then
// creates any missing service instances/keys and caches the credentials.
// Returns updated credentials and false if the user aborted.
func ensureXsuaaCredentials(ctx context.Context, plans []xsuaaRegionPlan, creds *store.Credentials, skipConfirm bool) (*store.Credentials, bool, error) {
	setupNeeded := false
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
		if err := xsuaaPrintSetupPreview(plans); err != nil {
			return creds, false, err
		}
		fmt.Fprint(os.Stderr, "Proceed with service/key creation? [y/N] ")
		text, ok := readLine(ctx)
		if !ok || strings.ToLower(text) != "y" {
			fmt.Fprintln(os.Stdout, "Aborted.")
			return creds, false, nil
		}
		fmt.Fprintln(os.Stdout)
	}

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

	if !anyPhase3 {
		return creds, true, nil
	}

	var err error
	creds, err = store.Load()
	if err != nil {
		return nil, false, fmt.Errorf("loading credentials: %w", err)
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
		client.SetTokenRefresher(makeTokenRefresher(plan.APIURL, tok.AccessToken))

		for oi := range plan.Orgs {
			op := &plan.Orgs[oi]
			if op.XsuaaReady || (!op.NeedsInstance && !op.NeedsKey && !op.NeedsFetch) {
				continue
			}

			if op.NeedsInstance {
				if plan.ServicePlan == nil {
					fmt.Fprintf(os.Stderr, "[%s] %s: service plan %s/%s not found — skipping\n",
						plan.Region, op.Org.Name, xsuaaServiceOffering, xsuaaServicePlan)
					continue
				}
				fmt.Fprintf(os.Stdout, "[%s] %s: creating service instance '%s'...\n",
					plan.Region, op.Org.Name, xsuaaInstanceName)
				if err := client.CreateServiceInstance(ctx, xsuaaInstanceName, op.UtilSpaceGUID, plan.ServicePlan.GUID); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] %s: failed to create service instance: %v\n",
						plan.Region, op.Org.Name, err)
					continue
				}

				fmt.Fprintln(os.Stdout, "Waiting 8 s for CF async processing...")
				select {
				case <-ctx.Done():
					return creds, false, ctx.Err()
				case <-time.After(8 * time.Second):
				}

				inst, err := client.FindServiceInstance(ctx, xsuaaInstanceName, op.UtilSpaceGUID)
				if err != nil || inst == nil {
					fmt.Fprintf(os.Stderr, "[%s] %s: could not find newly created service instance: %v\n",
						plan.Region, op.Org.Name, err)
					continue
				}
				op.InstanceGUID = inst.GUID
			}

			if op.NeedsKey {
				fmt.Fprintf(os.Stdout, "[%s] %s: creating service key '%s'...\n",
					plan.Region, op.Org.Name, xsuaaKeyName)
				if err := client.CreateServiceCredentialBinding(ctx, xsuaaKeyName, op.InstanceGUID); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] %s: failed to create service key: %v\n",
						plan.Region, op.Org.Name, err)
					continue
				}

				fmt.Fprintln(os.Stdout, "Waiting 8 s for CF async processing...")
				select {
				case <-ctx.Done():
					return creds, false, ctx.Err()
				case <-time.After(8 * time.Second):
				}

				key, err := client.FindServiceCredentialBinding(ctx, xsuaaKeyName, op.InstanceGUID)
				if err != nil || key == nil {
					fmt.Fprintf(os.Stderr, "[%s] %s: could not find newly created service key: %v\n",
						plan.Region, op.Org.Name, err)
					continue
				}
				op.KeyGUID = key.GUID
			}

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

	return creds, true, nil
}

// xsuaaRefreshToken returns current XSUAA credentials for orgGUID, refreshing
// the access token if absent or within 60 s of expiry. mu guards creds.
func xsuaaRefreshToken(ctx context.Context, orgGUID string, creds *store.Credentials, mu *sync.Mutex) (store.XsuaaData, error) {
	mu.Lock()
	xd := creds.OrgXsuaa[orgGUID]
	mu.Unlock()

	if xd.AccessToken != "" && !time.Now().Add(60*time.Second).After(xd.TokenExpiry) {
		return xd, nil
	}

	token, expiry, err := xsuaa.GetAccessToken(ctx, xd.URL, xd.ClientID, xd.ClientSecret)
	if err != nil {
		return xd, fmt.Errorf("XSUAA token: %w", err)
	}
	xd.AccessToken = token
	xd.TokenExpiry = expiry

	mu.Lock()
	creds.OrgXsuaa[orgGUID] = xd
	_ = store.Save(creds)
	mu.Unlock()

	return xd, nil
}

// xsuaaPrintSetupPreview renders a TOON preview of util spaces where the
// service instance or key will be created.
func xsuaaPrintSetupPreview(plans []xsuaaRegionPlan) error {
	var previewRegions []xsuaaSetupRegion
	for _, plan := range plans {
		pr := xsuaaSetupRegion{ID: plan.Region}
		for _, op := range plan.Orgs {
			if !op.NeedsInstance && !op.NeedsKey {
				continue
			}
			pr.Orgs = append(pr.Orgs, xsuaaSetupOrg{
				ID:   op.Org.GUID,
				Name: op.Org.Name,
				Spaces: []xsuaaSetupSpace{
					{ID: op.UtilSpaceGUID, Name: op.UtilSpaceName},
				},
			})
		}
		if len(pr.Orgs) > 0 {
			previewRegions = append(previewRegions, pr)
		}
	}

	out, err := toonenc.Marshal(xsuaaSetupDoc{Regions: previewRegions}, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding setup preview: %w", err)
	}
	fmt.Fprintln(os.Stdout, "The following service instance/key will be created in the 'util' space:")
	os.Stdout.Write(out)
	fmt.Fprintln(os.Stdout)
	return nil
}
