package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cis"
	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

type dscSubaccount struct {
	GUID              string `json:"guid"              toon:"guid"`
	DisplayName       string `json:"displayName"       toon:"displayName"`
	GlobalAccountGUID string `json:"globalAccountGUID" toon:"globalAccountGUID"`
	Subdomain         string `json:"subdomain"         toon:"subdomain"`
	Region            string `json:"region"            toon:"region"`
	State             string `json:"state"             toon:"state"`
	StateMessage      string `json:"stateMessage"      toon:"stateMessage"`
	BetaEnabled       bool   `json:"betaEnabled"       toon:"betaEnabled"`
	UsedForProduction string `json:"usedForProduction" toon:"usedForProduction"`
	Description       string `json:"description"       toon:"description"`
	CreatedDate       string `json:"createdDate"       toon:"createdDate"`
	ModifiedDate      string `json:"modifiedDate"      toon:"modifiedDate"`
	ParentGUID        string `json:"parentGUID"        toon:"parentGUID"`
	ParentType        string `json:"parentType"        toon:"parentType"`
}

type dscDestProp struct {
	Key   string `json:"key"   toon:"key"`
	Value string `json:"value" toon:"value"`
}

type dscDestination struct {
	Name       string        `json:"name"       toon:"name"`
	Properties []dscDestProp `json:"properties" toon:"properties"`
}

type dscServiceInstance struct {
	ID      string `json:"id"      toon:"id"`
	Name    string `json:"name"    toon:"name"`
	Service string `json:"service" toon:"service"`
	Plan    string `json:"plan"    toon:"plan"`
	State   string `json:"state"   toon:"state"`
}

type dscSpace struct {
	ID       string               `json:"space_id"   toon:"space_id"`
	Name     string               `json:"space_name" toon:"space_name"`
	Services []dscServiceInstance `json:"services"   toon:"services"`
}

type dscOutDoc struct {
	Subaccount      dscSubaccount         `json:"subaccount"      toon:"subaccount"`
	Spaces          []dscSpace            `json:"spaces"          toon:"spaces"`
	Destinations    []dscDestination      `json:"destinations"    toon:"destinations"`
	RoleCollections []rcOutRoleCollection `json:"rolecollections" toon:"rolecollections"`
}

// ── helpers ───────────────────────────────────────────────────────────────────

func cisSubaccountToOut(sa *cis.Subaccount) dscSubaccount {
	return dscSubaccount{
		GUID:              sa.GUID,
		DisplayName:       sa.DisplayName,
		GlobalAccountGUID: sa.GlobalAccountGUID,
		Subdomain:         sa.Subdomain,
		Region:            sa.Region,
		State:             sa.State,
		StateMessage:      sa.StateMessage,
		BetaEnabled:       sa.BetaEnabled,
		UsedForProduction: sa.UsedForProduction,
		Description:       sa.Description,
		CreatedDate:       xsuaa.MSToISO(sa.CreatedDate),
		ModifiedDate:      xsuaa.MSToISO(sa.ModifiedDate),
		ParentGUID:        sa.ParentGUID,
		ParentType:        sa.ParentType,
	}
}

func destMapToOut(d map[string]string) dscDestination {
	name := d["Name"]
	var props []dscDestProp
	for k, v := range d {
		if k == "Name" {
			continue
		}
		props = append(props, dscDestProp{Key: k, Value: v})
	}
	sort.Slice(props, func(i, j int) bool {
		return props[i].Key < props[j].Key
	})
	return dscDestination{Name: name, Properties: props}
}

// ── command ───────────────────────────────────────────────────────────────────

var describeSubaccountCmd = &cobra.Command{
	Use:   "describe-subaccount",
	Short: "Describe a BTP subaccount: metadata, destinations, and XSUAA role collections",
	Long: `Describe a BTP subaccount by looking up its metadata from the CIS Accounts Service,
listing its subaccount-level destinations, and listing its XSUAA role collections.

The --org flag identifies the CF organization that corresponds to the subaccount.
It is matched by exact GUID or case-insensitive substring on the org name.

CIS central-viewer service key credentials are auto-discovered from CF and cached
in ~/.bo/credentials.json for subsequent runs.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFilter, _ := cmd.Flags().GetString("org")
		subaccountFlag, _ := cmd.Flags().GetString("subaccount")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

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

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// ── Phase 1: find target org ──────────────────────────────────────────

		type targetOrg struct {
			Org    cf.Organization
			APIURL string
		}
		var target *targetOrg

		orgFilterLower := strings.ToLower(orgFilter)
		for _, apiURL := range apiURLs {
			tok, ok := creds.Tokens[apiURL]
			if !ok {
				regionName := store.APIURLToRegion(apiURL)
				fmt.Fprintf(os.Stderr, "[%s] no token — run: bo login --regions %s\n", regionName, regionName)
				continue
			}
			client := cf.NewClient(apiURL, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))

			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				regionName := store.APIURLToRegion(apiURL)
				fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, err)
				continue
			}

			for _, org := range orgs {
				if org.GUID == orgFilter || strings.Contains(strings.ToLower(org.Name), orgFilterLower) {
					o := org
					target = &targetOrg{Org: o, APIURL: apiURL}
					break
				}
			}
			if target != nil {
				break
			}
		}

		if target == nil {
			return fmt.Errorf("org %q not found", orgFilter)
		}

		// ── Phase 2: find/load CIS central-viewer key ─────────────────────────

		if creds.CISViewer == nil || creds.CISViewer.ClientID == "" {
			var cisData *store.CISViewerData
		outerCIS:
			for _, apiURL := range apiURLs {
				tok, ok := creds.Tokens[apiURL]
				if !ok {
					continue
				}
				client := cf.NewClient(apiURL, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))

				plan, err := client.FindServicePlan(ctx, "cis", "central-viewer")
				if err != nil || plan == nil {
					continue
				}

				instances, err := client.ListServiceInstancesByPlanGUID(ctx, plan.GUID, "")
				if err != nil {
					continue
				}

				for _, inst := range instances {
					key, err := client.FindAnyServiceCredentialBinding(ctx, inst.GUID)
					if err != nil || key == nil {
						continue
					}
					details, err := client.GetServiceCredentialDetails(ctx, key.GUID)
					if err != nil {
						continue
					}

					uaa, _ := details["uaa"].(map[string]interface{})
					endpoints, _ := details["endpoints"].(map[string]interface{})
					if uaa == nil || endpoints == nil {
						continue
					}

					clientID, _ := uaa["clientid"].(string)
					clientSecret, _ := uaa["clientsecret"].(string)
					uaaURL, _ := uaa["url"].(string)
					accountsURL, _ := endpoints["accounts_service_url"].(string)

					if clientID == "" || clientSecret == "" || uaaURL == "" || accountsURL == "" {
						continue
					}

					cisData = &store.CISViewerData{
						ClientID:           clientID,
						ClientSecret:       clientSecret,
						TokenURL:           uaaURL,
						AccountsServiceURL: accountsURL,
					}
					break outerCIS
				}
			}

			if cisData == nil {
				return fmt.Errorf("CIS central-viewer service key not found\n" +
					"Create a 'cis' service instance with plan 'central-viewer' in any accessible org/space\n" +
					"and ensure a service key exists for it")
			}

			creds.CISViewer = cisData
			if err := store.Save(creds); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save CIS credentials: %v\n", err)
			}
		}

		cisData := creds.CISViewer

		// ── Phase 3: refresh CIS token if needed ──────────────────────────────

		if cisData.AccessToken == "" || time.Now().Add(60*time.Second).After(cisData.TokenExpiry) {
			token, expiry, err := cis.GetAccessToken(ctx, cisData.TokenURL, cisData.ClientID, cisData.ClientSecret)
			if err != nil {
				return fmt.Errorf("CIS token: %w", err)
			}
			cisData.AccessToken = token
			cisData.TokenExpiry = expiry
			creds.CISViewer = cisData
			if err := store.Save(creds); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save CIS token: %v\n", err)
			}
		}

		// ── Phase 4: get subaccount details ───────────────────────────────────

		subaccountID := target.Org.GUID
		if subaccountFlag != "" {
			subaccountID = subaccountFlag
		}

		sa, err := cis.GetSubaccount(ctx, cisData.AccountsServiceURL, cisData.AccessToken, subaccountID)
		if err != nil {
			return fmt.Errorf("getting subaccount %s: %w", subaccountID, err)
		}

		// ── Phase 4.5: list spaces and their service instances ────────────────

		var spaces []dscSpace
		func() {
			tok, ok := creds.Tokens[target.APIURL]
			if !ok {
				return
			}
			targetClient := cf.NewClient(target.APIURL, tok.AccessToken)
			targetClient.SetTokenRefresher(makeTokenRefresher(target.APIURL, tok.AccessToken))

			cfSpaces, err := targetClient.ListOrganizationSpaces(ctx, target.Org.GUID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: listing spaces: %v\n", err)
				return
			}

			instances, err := targetClient.ListServiceInstancesByOrg(ctx, target.Org.GUID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: listing service instances: %v\n", err)
				return
			}

			planGUIDSet := make(map[string]struct{})
			for _, inst := range instances {
				if g := inst.Relationships.ServicePlan.Data.GUID; g != "" {
					planGUIDSet[g] = struct{}{}
				}
			}
			planGUIDs := make([]string, 0, len(planGUIDSet))
			for g := range planGUIDSet {
				planGUIDs = append(planGUIDs, g)
			}

			planDetails, err := targetClient.ListServicePlanDetails(ctx, planGUIDs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: fetching service plan details: %v\n", err)
			}

			bySpace := make(map[string][]dscServiceInstance, len(cfSpaces))
			for _, inst := range instances {
				spaceGUID := inst.Relationships.Space.Data.GUID
				planGUID := inst.Relationships.ServicePlan.Data.GUID
				svc := dscServiceInstance{
					ID:    inst.GUID,
					Name:  inst.Name,
					State: inst.LastOperation.State,
				}
				if pd, ok := planDetails[planGUID]; ok {
					svc.Service = pd.ServiceName
					svc.Plan = pd.Name
				}
				bySpace[spaceGUID] = append(bySpace[spaceGUID], svc)
			}

			sort.Slice(cfSpaces, func(i, j int) bool { return cfSpaces[i].Name < cfSpaces[j].Name })
			for _, sp := range cfSpaces {
				svcs := bySpace[sp.GUID]
				sort.Slice(svcs, func(i, j int) bool {
					if svcs[i].Service != svcs[j].Service {
						return svcs[i].Service < svcs[j].Service
					}
					return svcs[i].Name < svcs[j].Name
				})
				spaces = append(spaces, dscSpace{
					ID:       sp.GUID,
					Name:     sp.Name,
					Services: svcs,
				})
			}
		}()

		// ── Phase 5: find destination service in target org ───────────────────

		var destinations []dscDestination

		func() {
			tok, ok := creds.Tokens[target.APIURL]
			if !ok {
				fmt.Fprintf(os.Stderr, "warning: no token for target org region — skipping destinations\n")
				return
			}
			targetClient := cf.NewClient(target.APIURL, tok.AccessToken)
			targetClient.SetTokenRefresher(makeTokenRefresher(target.APIURL, tok.AccessToken))

			destPlan, err := targetClient.FindServicePlan(ctx, "destination", "lite")
			if err != nil || destPlan == nil {
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: looking up destination service plan: %v\n", err)
				}
				return
			}

			instances, err := targetClient.ListServiceInstancesByPlanGUID(ctx, destPlan.GUID, target.Org.GUID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: listing destination instances: %v\n", err)
				return
			}

			for _, inst := range instances {
				key, err := targetClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
				if err != nil || key == nil {
					continue
				}
				details, err := targetClient.GetServiceCredentialDetails(ctx, key.GUID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: fetching destination key credentials: %v\n", err)
					continue
				}

				clientID, _ := details["clientid"].(string)
				clientSecret, _ := details["clientsecret"].(string)
				tokenURL, _ := details["url"].(string)
				destURI, _ := details["uri"].(string)

				if clientID == "" || clientSecret == "" || tokenURL == "" || destURI == "" {
					fmt.Fprintf(os.Stderr, "warning: incomplete destination service key credentials\n")
					continue
				}

				destToken, _, err := destination.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: getting destination token: %v\n", err)
					continue
				}

				rawDests, err := destination.ListSubaccountDestinations(ctx, destURI, destToken)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: listing destinations: %v\n", err)
					continue
				}

				for _, d := range rawDests {
					destinations = append(destinations, destMapToOut(d))
				}
				sort.Slice(destinations, func(i, j int) bool {
					return destinations[i].Name < destinations[j].Name
				})
				return
			}
		}()

		// ── Phase 6: get XSUAA role collections ───────────────────────────────

		var roleCollections []rcOutRoleCollection

		func() {
			includeTargetOrg := cosOrgSet{cosOrgRef{ID: target.Org.GUID}}
			xsuaaPlans := discoverXsuaaPlans(ctx, []string{target.APIURL}, creds, includeTargetOrg, nil)

			updatedCreds, proceed, err := ensureXsuaaCredentials(ctx, xsuaaPlans, creds, skipConfirm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: XSUAA setup: %v\n", err)
				return
			}
			if !proceed {
				return
			}
			creds = updatedCreds

			reloadedCreds, err := store.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: reloading credentials: %v\n", err)
				return
			}
			creds = reloadedCreds

			var mu sync.Mutex
			xd, err := xsuaaRefreshToken(ctx, target.Org.GUID, creds, &mu)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: XSUAA token: %v\n", err)
				return
			}

			regionName := store.APIURLToRegion(target.APIURL)
			apiBaseURL := xsuaa.ResolveAPIBaseURL(xd.APIURL, regionName)

			rcs, err := xsuaa.ListRoleCollections(ctx, apiBaseURL, xd.AccessToken)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: listing role collections: %v\n", err)
				return
			}

			for _, rc := range rcs {
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
				roleCollections = append(roleCollections, rcOutRoleCollection{
					Name:           rc.Name,
					Description:    rc.Description,
					IsReadOnly:     rc.IsReadOnly,
					RoleReferences: refs,
				})
			}
			sort.Slice(roleCollections, func(i, j int) bool {
				return roleCollections[i].Name < roleCollections[j].Name
			})
		}()

		// ── Phase 7: output ───────────────────────────────────────────────────

		doc := dscOutDoc{
			Subaccount:      cisSubaccountToOut(sa),
			Spaces:          spaces,
			Destinations:    destinations,
			RoleCollections: roleCollections,
		}

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
	rootCmd.AddCommand(describeSubaccountCmd)
	describeSubaccountCmd.Flags().String("org", "", "Org name (case-insensitive substring) or GUID to describe (required)")
	describeSubaccountCmd.MarkFlagRequired("org")
	describeSubaccountCmd.Flags().String("subaccount", "", "BTP subaccount GUID to use in the CIS API query (defaults to the CF org GUID)")
	describeSubaccountCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	describeSubaccountCmd.Flags().String("format", "toon", "Output format: toon (default) or json")
	describeSubaccountCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt for XSUAA service/key creation")
}
