package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/html5apps"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

type html5AppRow struct {
	Region                     string `json:"region"                         toon:"region"`
	OrgID                      string `json:"org_id"                         toon:"org_id"`
	OrgName                    string `json:"org_name"                       toon:"org_name"`
	SpaceID                    string `json:"space_id"                       toon:"space_id"`
	SpaceName                  string `json:"space_name"                     toon:"space_name"`
	AppHostServiceInstanceName string `json:"app_host_service_instance_name" toon:"app_host_service_instance_name"`
	AppHostServiceInstanceID   string `json:"app_host_service_instance_id"   toon:"app_host_service_instance_id"`

	// The following three are only populated with --from-destinations: the
	// destination record itself (DestinationName) and, when it was found at
	// instance level, the destination service instance that hosts it.
	// DestinationInstanceName/ID are left empty for a subaccount-level
	// destination, since there is no single instance to attribute it to.
	DestinationInstanceName string `json:"destination_instance_name,omitempty" toon:"destination_instance_name,omitempty"`
	DestinationInstanceID   string `json:"destination_instance_id,omitempty"   toon:"destination_instance_id,omitempty"`
	DestinationName         string `json:"destination_name,omitempty"          toon:"destination_name,omitempty"`

	ApplicationName    string `json:"application_name"               toon:"application_name"`
	ApplicationVersion string `json:"application_version"            toon:"application_version"`
	ChangedOn          string `json:"changed_on"                     toon:"changed_on"`
	CreatedOn          string `json:"created_on"                     toon:"created_on"`
	IsDefault          bool   `json:"is_default"                     toon:"is_default"`
	Public             bool   `json:"public"                         toon:"public"`
}

type html5AppsDoc struct {
	Apps []html5AppRow `json:"apps" toon:"apps"`
}

// ── service-instance based discovery ────────────────────────────────────────

// html5RegionResult holds one region's orgs/spaces plus the
// html5-apps-repo/app-runtime plan GUID found in that region (empty if the
// plan isn't offered there).
type html5RegionResult struct {
	Region   string
	Client   *cf.Client
	Orgs     []cf.Organization
	Spaces   []cf.Space
	PlanGUID string
	Err      error
}

// resolveHTML5OrgsSpaces lists orgs/spaces (filtered per org/orgGUID/
// includeOrgs/excludeOrgs, same rules as 'bo apps') and looks up the
// html5-apps-repo/app-runtime service plan, once per region, in parallel.
func resolveHTML5OrgsSpaces(ctx context.Context, creds *store.Credentials, apiURLs []string, orgGUID string, includeOrgs, excludeOrgs cosOrgSet) []html5RegionResult {
	results := make([]html5RegionResult, len(apiURLs))
	var wg sync.WaitGroup
	for i, apiURL := range apiURLs {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			regionName := store.APIURLToRegion(url)

			tok, ok := creds.Tokens[url]
			if !ok {
				results[idx] = html5RegionResult{Region: regionName,
					Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
				return
			}
			client := cf.NewClient(url, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				results[idx] = html5RegionResult{Region: regionName, Client: client,
					Err: fmt.Errorf("listing orgs: %w", err)}
				return
			}

			var filteredOrgs []cf.Organization
			for _, org := range orgs {
				if orgGUID != "" && org.GUID != orgGUID {
					continue
				}
				if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
					continue
				}
				if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
					continue
				}
				filteredOrgs = append(filteredOrgs, org)
			}
			if len(filteredOrgs) == 0 {
				results[idx] = html5RegionResult{Region: regionName, Client: client}
				return
			}

			orgGUIDs := make([]string, len(filteredOrgs))
			for i, o := range filteredOrgs {
				orgGUIDs[i] = o.GUID
			}
			spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
			if err != nil {
				results[idx] = html5RegionResult{Region: regionName, Client: client,
					Err: fmt.Errorf("listing spaces: %w", err)}
				return
			}

			var planGUID string
			if plan, planErr := client.FindServicePlan(ctx, "html5-apps-repo", "app-runtime"); planErr != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] looking up html5-apps-repo/app-runtime plan: %v\n", regionName, planErr)
			} else if plan != nil {
				planGUID = plan.GUID
			}

			results[idx] = html5RegionResult{
				Region: regionName, Client: client,
				Orgs: filteredOrgs, Spaces: spaces, PlanGUID: planGUID,
			}
		}(i, apiURL)
	}
	wg.Wait()
	return results
}

// html5AutoKeyName is the service key name used both in the manual "cf
// create-service-key" instructions shown interactively and by
// --no-key-option=create's automatic creation, so the two stay in sync.
const html5AutoKeyName = "bo-html5-key"

// html5KeyOption is the normalized value of --no-key-option, governing what
// happens for every html5-apps-repo/app-runtime instance found with no
// service key during one run (see normalizeHTML5KeyOption).
type html5KeyOption string

const (
	html5KeyOptionPrompt html5KeyOption = ""       // interactive retry/skip/abort prompt (default)
	html5KeyOptionSkip   html5KeyOption = "skip"   // warn and skip, no prompt
	html5KeyOptionCreate html5KeyOption = "create" // create a service key automatically
	html5KeyOptionAbort  html5KeyOption = "abort"  // stop the whole command immediately
)

// normalizeHTML5KeyOption validates and normalizes --no-key-option's raw
// value (accepting both the short and long form of each option).
func normalizeHTML5KeyOption(raw string) (html5KeyOption, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return html5KeyOptionPrompt, nil
	case "s", "skip":
		return html5KeyOptionSkip, nil
	case "c", "create":
		return html5KeyOptionCreate, nil
	case "a", "abort":
		return html5KeyOptionAbort, nil
	default:
		return "", fmt.Errorf("invalid --no-key-option %q: must be one of s/skip, c/create, a/abort", raw)
	}
}

// resolveHTML5InstanceToken obtains (or reuses a cached) OAuth token for one
// html5-apps-repo/app-runtime service instance, following the same
// find-service-key / fetch-credentials / get-token flow as
// resolveSpaceDestClients, cached under creds.SpaceHTML5Services. Returns
// ok=false (with a warning already printed) when the instance has to be
// skipped, or a non-nil error if the whole command must stop — either the
// user cancelled an interactive retry prompt (errAborted) or chose
// --no-key-option=abort.
func resolveHTML5InstanceToken(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, spaceGUID string, inst cf.ServiceInstance, creds *store.Credentials, keyOption html5KeyOption) (sdDestClient, bool, error) {
	if creds.SpaceHTML5Services == nil {
		creds.SpaceHTML5Services = make(map[string]map[string]*store.DestInstanceCache)
	}
	if creds.SpaceHTML5Services[spaceGUID] == nil {
		creds.SpaceHTML5Services[spaceGUID] = make(map[string]*store.DestInstanceCache)
	}
	spaceCache := creds.SpaceHTML5Services[spaceGUID]
	cached := spaceCache[inst.GUID]
	needToken := cached == nil || cached.AccessToken == "" ||
		time.Now().Add(60*time.Second).After(cached.TokenExpiry)

	if needToken {
		key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
		if keyErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: finding service key for %q: %v\n", inst.Name, keyErr)
			return sdDestClient{}, false, nil
		}
		if key == nil {
			switch keyOption {
			case html5KeyOptionSkip:
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: no service key for html5-apps-repo instance %q — skipping\n", inst.Name)
				return sdDestClient{}, false, nil

			case html5KeyOptionAbort:
				return sdDestClient{}, false, fmt.Errorf(
					"no service key for html5-apps-repo instance %q (%s) — aborting (--no-key-option=abort)",
					inst.Name, inst.GUID)

			case html5KeyOptionCreate:
				fmt.Fprintf(cmd.ErrOrStderr(), "creating service key %q for html5-apps-repo instance %q...\n", html5AutoKeyName, inst.Name)
				bindingGUID, createErr := cfClient.CreateServiceCredentialBinding(ctx, html5AutoKeyName, inst.GUID)
				if createErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: creating service key for %q: %v — skipping\n", inst.Name, createErr)
					return sdDestClient{}, false, nil
				}
				key = &cf.ServiceCredentialBinding{GUID: bindingGUID, Name: html5AutoKeyName}

			default: // html5KeyOptionPrompt
				for key == nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\nWARNING: No service key found for html5-apps-repo/app-runtime instance %q (%s)\n"+
							"  Create one manually, e.g. via CF CLI:\n"+
							"    cf create-service-key %s %s\n"+
							"  Then press Enter to retry, type 's' to skip this instance, or Ctrl-C to abort.\n"+
							"  (Pass --no-key-option=skip/create/abort to apply one choice to every instance instead.)\n",
						inst.Name, inst.GUID, inst.Name, html5AutoKeyName)
					retry, skip := promptRetryOrSkip(ctx)
					if !retry && !skip {
						return sdDestClient{}, false, errAborted
					}
					if skip {
						return sdDestClient{}, false, nil
					}
					key, keyErr = cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
					if keyErr != nil || key == nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: still no service key for %q\n", inst.Name)
						key = nil
					}
				}
			}
		}

		details, detailErr := cfClient.GetServiceCredentialDetails(ctx, key.GUID)
		if detailErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: fetching key details for %q: %v\n", inst.Name, detailErr)
			return sdDestClient{}, false, nil
		}
		clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
		if missing != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: incomplete credentials in service key for %q (missing %q)\n", inst.Name, missing)
			return sdDestClient{}, false, nil
		}

		newToken, expiry, tokErr := html5apps.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
		if tokErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: token for html5-apps-repo instance %q: %v\n", inst.Name, tokErr)
			return sdDestClient{}, false, nil
		}

		if cached == nil {
			cached = &store.DestInstanceCache{}
			spaceCache[inst.GUID] = cached
		}
		cached.InstanceName = inst.Name
		cached.TokenURL = tokenURL
		cached.URI = uri
		cached.AccessToken = newToken
		cached.TokenExpiry = expiry
	}

	if cached == nil || cached.URI == "" {
		return sdDestClient{}, false, nil
	}
	return sdDestClient{
		InstanceGUID: inst.GUID,
		InstanceName: cached.InstanceName,
		URI:          cached.URI,
		Token:        cached.AccessToken,
	}, true, nil
}

// saveHTML5Cache persists creds.SpaceHTML5Services the same way
// saveDestCache persists SpaceDestServices — reloading from disk first so a
// concurrent CF token refresh isn't clobbered.
func saveHTML5Cache(cmd *cobra.Command, creds *store.Credentials) {
	fresh, err := store.Load()
	if err != nil {
		fresh = creds
	} else {
		fresh.SpaceHTML5Services = creds.SpaceHTML5Services
	}
	if saveErr := store.Save(fresh); saveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving html5-apps-repo credentials: %v\n", saveErr)
	}
}

func html5RowsFromInstanceClient(cmd *cobra.Command, ctx context.Context, c sdDestClient, region, orgID, orgName, spaceID, spaceName string) []html5AppRow {
	apps, err := html5apps.ListApplicationsMetadata(ctx, c.URI, c.Token)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: instance %q: listing applications metadata: %v\n",
			region, orgName, spaceName, c.InstanceName, err)
		return nil
	}
	rows := make([]html5AppRow, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, html5AppRow{
			Region: region, OrgID: orgID, OrgName: orgName,
			SpaceID: spaceID, SpaceName: spaceName,
			AppHostServiceInstanceName: c.InstanceName, AppHostServiceInstanceID: c.InstanceGUID,
			ApplicationName: a.ApplicationName, ApplicationVersion: a.ApplicationVersion,
			ChangedOn: a.ChangedOn, CreatedOn: a.CreatedOn,
			IsDefault: a.IsDefault, Public: a.Public,
		})
	}
	return rows
}

// matchesSpaceFilter reports whether sp is selected by --spaces (a
// case-insensitive set of space names and/or GUIDs). A nil filter means
// "every space in scope".
func matchesSpaceFilter(filter map[string]bool, sp cf.Space) bool {
	if filter == nil {
		return true
	}
	return filter[strings.ToLower(sp.Name)] || filter[strings.ToLower(sp.GUID)]
}

// fetchHTML5ViaServiceInstances is the default discovery path: for every org/
// space in scope, find html5-apps-repo/app-runtime service instances, obtain
// a service-key-derived OAuth token for each (per keyOption when none
// exists — see resolveHTML5InstanceToken), and fetch
// GET /applications/metadata from each.
func fetchHTML5ViaServiceInstances(ctx context.Context, cmd *cobra.Command, creds *store.Credentials, apiURLs []string, orgGUID string, includeOrgs, excludeOrgs cosOrgSet, spacesFilter map[string]bool, keyOption html5KeyOption) ([]html5AppRow, error) {
	regionResults := resolveHTML5OrgsSpaces(ctx, creds, apiURLs, orgGUID, includeOrgs, excludeOrgs)

	var rows []html5AppRow
	for _, rr := range regionResults {
		if rr.Err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %v\n", rr.Region, rr.Err)
			continue
		}
		if rr.Client == nil || len(rr.Spaces) == 0 {
			continue
		}
		if rr.PlanGUID == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] html5-apps-repo/app-runtime service plan not found — skipping region\n", rr.Region)
			continue
		}

		orgByGUID := make(map[string]string, len(rr.Orgs))
		for _, o := range rr.Orgs {
			orgByGUID[o.GUID] = o.Name
		}

		for _, sp := range rr.Spaces {
			if !matchesSpaceFilter(spacesFilter, sp) {
				continue
			}
			spOrgGUID := sp.Relationships.Organization.Data.GUID
			spOrgName := orgByGUID[spOrgGUID]

			instances, err := rr.Client.ListServiceInstancesInSpace(ctx, sp.GUID, rr.PlanGUID)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: listing html5-apps-repo instances: %v\n",
					rr.Region, spOrgName, sp.Name, err)
				continue
			}
			for _, inst := range instances {
				c, ok, resolveErr := resolveHTML5InstanceToken(ctx, cmd, rr.Client, sp.GUID, inst, creds, keyOption)
				if resolveErr != nil {
					saveHTML5Cache(cmd, creds)
					return rows, resolveErr
				}
				if !ok {
					continue
				}
				rows = append(rows, html5RowsFromInstanceClient(cmd, ctx, c, rr.Region, spOrgGUID, spOrgName, sp.GUID, sp.Name)...)
			}
		}
	}

	saveHTML5Cache(cmd, creds)
	return rows, nil
}

// ── destination based discovery (--from-destinations) ──────────────────────

// destPropCI returns dest's value for key, matched case-insensitively
// against dest's own keys (destination property casing varies: "URL" vs
// "url", "clientId" vs "ClientId", ...).
func destPropCI(dest map[string]string, key string) string {
	kl := strings.ToLower(key)
	for k, v := range dest {
		if strings.ToLower(k) == kl {
			return v
		}
	}
	return ""
}

// findOrgAndSpaces looks up an org by GUID (exact) or name (case-insensitive
// substring) across apiURLs — the same matching rule resolveOrgDestClient
// uses — and returns its region, identity, and spaces.
func findOrgAndSpaces(ctx context.Context, cmd *cobra.Command, orgFlag string, creds *store.Credentials, apiURLs []string) (region, orgGUID, orgName string, spaces []cf.Space, err error) {
	fl := strings.ToLower(orgFlag)
	for _, apiURL := range apiURLs {
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			continue
		}
		c := cf.NewClient(apiURL, tok.AccessToken)
		c.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
		orgs, listErr := c.ListOrganizations(ctx)
		if listErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing orgs: %v\n", store.APIURLToRegion(apiURL), listErr)
			continue
		}
		for _, org := range orgs {
			if strings.EqualFold(org.GUID, orgFlag) || strings.Contains(strings.ToLower(org.Name), fl) {
				sp, spErr := c.ListOrganizationSpaces(ctx, org.GUID)
				if spErr != nil {
					return store.APIURLToRegion(apiURL), org.GUID, org.Name, nil, fmt.Errorf("listing spaces in org %q: %w", org.Name, spErr)
				}
				return store.APIURLToRegion(apiURL), org.GUID, org.Name, sp, nil
			}
		}
	}
	return "", "", "", nil, fmt.Errorf("org %q not found in any accessible region", orgFlag)
}

// html5RowsFromDestination inspects one destination and, if it carries both
// an "app_host_id" and a "sap.cloud.service" property (the marker for a
// destination exposing one app host's content), uses its OAuth2 client
// credentials to fetch and return that app host's application metadata.
// Returns nil if the destination doesn't match or the fetch fails (a warning
// is printed in the latter case). destInstanceName/destInstanceID identify
// the destination service instance the destination was found in — pass ""
// for both when dest is a subaccount-level destination.
func html5RowsFromDestination(ctx context.Context, cmd *cobra.Command, dest map[string]string, region, orgID, orgName, spaceID, spaceName, destInstanceName, destInstanceID string) []html5AppRow {
	appHostID := destPropCI(dest, "app_host_id")
	cloudService := destPropCI(dest, "sap.cloud.service")
	if appHostID == "" || cloudService == "" {
		return nil
	}

	name := destPropCI(dest, "Name")
	uri := destPropCI(dest, "URL")
	clientID := destPropCI(dest, "clientId")
	clientSecret := destPropCI(dest, "clientSecret")
	tokenURL := destPropCI(dest, "tokenServiceURL")
	if uri == "" || clientID == "" || clientSecret == "" || tokenURL == "" {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: destination %q has app_host_id/sap.cloud.service but is missing URL/clientId/clientSecret/tokenServiceURL — skipping\n", name)
		return nil
	}

	token, _, tokErr := html5apps.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
	if tokErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: destination %q: getting token: %v\n", name, tokErr)
		return nil
	}
	apps, fetchErr := html5apps.ListApplicationsMetadata(ctx, uri, token)
	if fetchErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: destination %q: listing applications metadata: %v\n", name, fetchErr)
		return nil
	}

	rows := make([]html5AppRow, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, html5AppRow{
			Region: region, OrgID: orgID, OrgName: orgName,
			SpaceID: spaceID, SpaceName: spaceName,
			AppHostServiceInstanceName: name, AppHostServiceInstanceID: appHostID,
			DestinationInstanceName: destInstanceName, DestinationInstanceID: destInstanceID, DestinationName: name,
			ApplicationName: a.ApplicationName, ApplicationVersion: a.ApplicationVersion,
			ChangedOn: a.ChangedOn, CreatedOn: a.CreatedOn,
			IsDefault: a.IsDefault, Public: a.Public,
		})
	}
	return rows
}

// fetchHTML5ViaDestinations is the --from-destinations discovery path: for
// every target org, it inspects both subaccount-level destinations and
// every space's instance-level destinations, extracting application
// metadata from whichever ones carry an app_host_id/sap.cloud.service pair
// (see html5RowsFromDestination), instead of going through service
// instance keys directly.
func fetchHTML5ViaDestinations(ctx context.Context, cmd *cobra.Command, creds *store.Credentials, apiURLs []string, orgTargets []string, spacesFilter map[string]bool, noPrompt bool) ([]html5AppRow, error) {
	var rows []html5AppRow
	for _, target := range orgTargets {
		region, orgGUID, orgName, spaces, err := findOrgAndSpaces(ctx, cmd, target, creds, apiURLs)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			continue
		}

		// Subaccount-level destinations.
		_, _, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt)
		if resolveErr != nil {
			if errors.Is(resolveErr, errAborted) {
				return rows, errAborted
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
		} else {
			rawDests, fetchErr := destination.ListSubaccountDestinationsFull(ctx, destClient.URI, destClient.Token)
			if fetchErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing subaccount destinations: %v\n", orgName, fetchErr)
			} else {
				for _, raw := range rawDests {
					rows = append(rows, html5RowsFromDestination(ctx, cmd, raw, region, orgGUID, orgName, "", "", "", "")...)
				}
			}
		}

		// Instance-level destinations, per space.
		for _, sp := range spaces {
			if !matchesSpaceFilter(spacesFilter, sp) {
				continue
			}
			_, clients, spErr := resolveSpaceDestClients(ctx, cmd, sp.GUID, creds, apiURLs)
			if spErr != nil {
				if errors.Is(spErr, errAborted) {
					return rows, errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] %v\n", orgName, sp.Name, spErr)
				continue
			}
			for _, c := range clients {
				rawDests, fetchErr := destination.ListInstanceDestinationsFull(ctx, c.URI, c.Token)
				if fetchErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] instance %q: listing destinations: %v\n",
						orgName, sp.Name, c.InstanceName, fetchErr)
					continue
				}
				for _, raw := range rawDests {
					rows = append(rows, html5RowsFromDestination(ctx, cmd, raw, region, orgGUID, orgName, sp.GUID, sp.Name, c.InstanceName, c.InstanceGUID)...)
				}
			}
		}
	}
	return rows, nil
}

// ── rendering ────────────────────────────────────────────────────────────────

func renderHTML5AppsToon(w io.Writer, doc html5AppsDoc, color bool) error {
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

func renderHTML5AppsJSON(w io.Writer, doc html5AppsDoc) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func renderHTML5AppsCSV(w io.Writer, doc html5AppsDoc, color bool) error {
	csvW := newRainbowCSVWriter(w, color)
	defer csvW.Flush()

	if err := csvW.Write([]string{
		"region", "org_id", "org_name", "space_id", "space_name",
		"app_host_service_instance_name", "app_host_service_instance_id",
		"destination_instance_name", "destination_instance_id", "destination_name",
		"application_name", "application_version",
		"changed_on", "created_on", "is_default", "public",
	}); err != nil {
		return err
	}
	for _, a := range doc.Apps {
		if err := csvW.Write([]string{
			a.Region, a.OrgID, a.OrgName, a.SpaceID, a.SpaceName,
			a.AppHostServiceInstanceName, a.AppHostServiceInstanceID,
			a.DestinationInstanceName, a.DestinationInstanceID, a.DestinationName,
			a.ApplicationName, a.ApplicationVersion,
			a.ChangedOn, a.CreatedOn, fmt.Sprintf("%t", a.IsDefault), fmt.Sprintf("%t", a.Public),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ── command ───────────────────────────────────────────────────────────────────

var html5AppsCmd = &cobra.Command{
	Use:   "html5-apps",
	Short: "List HTML5 application metadata from HTML5 Application Repository service instances",
	Long: `List the applications stored in every accessible HTML5 Application Repository
Service (html5-apps-repo) app-runtime service instance, across one or more
regions and organizations.

For each matching service instance, an OAuth2 client_credentials token is
obtained from its service key (creating one interactively if none exists,
following the same flow as the SAP/cf-html5-apps-repo-cli-plugin CF CLI
plugin) and GET {uri}/applications/metadata is called to list its
applications.

The result is a flat list, one row per application, with columns: region,
org_id, org_name, space_id, space_name, app_host_service_instance_name,
app_host_service_instance_id, destination_instance_name,
destination_instance_id, destination_name, application_name,
application_version, changed_on, created_on, is_default, public. The
destination_* columns are only populated with --from-destinations (see
below).

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows, one per application

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. Use --spaces to further narrow to specific space names or
GUIDs (comma-separated, case-insensitive).

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a row matches if org_name, space_name,
app_host_service_instance_name, destination_instance_name,
destination_name, application_name, or application_version contains any of
them (case-insensitive).

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise
(including no --output) the default toon applies. Pass --format explicitly
to override the extension.

Use --from-destinations to discover applications via destinations instead of
service instance keys: subaccount-level and instance-level destinations in
the targeted orgs/spaces are scanned for ones carrying both an "app_host_id"
and a "sap.cloud.service" property, and each such destination's own OAuth2
client credentials (clientId/clientSecret/tokenServiceURL/URL) are used to
fetch that one app host's metadata directly — app_host_service_instance_name
and app_host_service_instance_id are then the destination's own Name and
app_host_id, since no CF service instance is involved in this path.
destination_name is always the destination's own Name; for an
instance-level destination, destination_instance_name/id additionally
identify the destination service instance it was found in, while a
subaccount-level destination leaves both empty (there is no single
instance to attribute it to).

--no-key-option <s/skip|c/create|a/abort> applies one choice to every
html5-apps-repo/app-runtime instance found with no service key during this
run, instead of prompting interactively for each one: skip warns and moves
on, create automatically runs the equivalent of
'cf create-service-key <instance> bo-html5-key', and abort stops the whole
command the first time it happens. Omit it to keep the interactive
retry/skip/abort prompt (the default). This only governs the service-key
decision for html5-apps-repo instances (the default, non
--from-destinations path) — --no-prompt below is the equivalent for
destination-resolution prompts under --from-destinations.

--no-prompt skips interactive destination-resolution prompts encountered
under --from-destinations, warning and skipping instead — useful for
non-interactive/CI runs.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgGUID, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		outputFile, _ := cmd.Flags().GetString("output")
		format, _ := cmd.Flags().GetString("format")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		fromDestinations, _ := cmd.Flags().GetBool("from-destinations")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
		keyOptionFlag, _ := cmd.Flags().GetString("no-key-option")
		keyOption, err := normalizeHTML5KeyOption(keyOptionFlag)
		if err != nil {
			return err
		}

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

		if orgGUID == "" && orgsFile == "" {
			includeOrgs, err = resolveDefaultOrgScope(creds)
			if err != nil {
				return err
			}
		}

		var spacesFilter map[string]bool
		if spacesFlag != "" {
			spacesFilter = make(map[string]bool)
			for _, s := range splitCSV(spacesFlag) {
				spacesFilter[strings.ToLower(s)] = true
			}
		}

		var rows []html5AppRow
		if fromDestinations {
			orgTargets, targetsErr := resolveOrgTargets(creds, orgGUID, orgsFile)
			if targetsErr != nil {
				return targetsErr
			}
			rows, err = fetchHTML5ViaDestinations(ctx, cmd, creds, apiURLs, orgTargets, spacesFilter, noPrompt)
		} else {
			rows, err = fetchHTML5ViaServiceInstances(ctx, cmd, creds, apiURLs, orgGUID, includeOrgs, excludeOrgs, spacesFilter, keyOption)
		}
		if err != nil {
			if errors.Is(err, errAborted) {
				return errAborted
			}
			return err
		}

		filtered := make([]html5AppRow, 0, len(rows))
		for _, r := range rows {
			fields := []string{
				r.OrgName, r.SpaceName, r.AppHostServiceInstanceName,
				r.DestinationInstanceName, r.DestinationName,
				r.ApplicationName, r.ApplicationVersion,
			}
			if includePattern != "" && !skipMatches(includePattern, fields...) {
				continue
			}
			if excludePattern != "" && skipMatches(excludePattern, fields...) {
				continue
			}
			filtered = append(filtered, r)
		}
		rows = filtered

		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Region != rows[j].Region {
				return rows[i].Region < rows[j].Region
			}
			if rows[i].OrgName != rows[j].OrgName {
				return rows[i].OrgName < rows[j].OrgName
			}
			if rows[i].SpaceName != rows[j].SpaceName {
				return rows[i].SpaceName < rows[j].SpaceName
			}
			if rows[i].AppHostServiceInstanceName != rows[j].AppHostServiceInstanceName {
				return rows[i].AppHostServiceInstanceName < rows[j].AppHostServiceInstanceName
			}
			return rows[i].ApplicationName < rows[j].ApplicationName
		})

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		doc := html5AppsDoc{Apps: rows}
		switch strings.ToLower(format) {
		case "json":
			return renderHTML5AppsJSON(out, doc)
		case "csv":
			return renderHTML5AppsCSV(out, doc, color)
		default:
			return renderHTML5AppsToon(out, doc, color)
		}
	},
}

func init() {
	html5AppsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(html5AppsCmd)
	html5AppsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	html5AppsCmd.Flags().String("org", "", "Org GUID (exact) or name (substring) to target")
	html5AppsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	html5AppsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name) — ignored with --from-destinations")
	html5AppsCmd.Flags().String("spaces", "", "Comma-separated space names or GUIDs to narrow down to (default: every space in scope)")
	html5AppsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	html5AppsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	html5AppsCmd.Flags().String("include", "", "Only include rows where org_name, space_name, app_host_service_instance_name, destination_instance_name, destination_name, application_name, or application_version contain any of these comma-separated keywords")
	html5AppsCmd.Flags().String("exclude", "", "Exclude rows where org_name, space_name, app_host_service_instance_name, destination_instance_name, destination_name, application_name, or application_version contain any of these comma-separated keywords")
	html5AppsCmd.Flags().Bool("from-destinations", false, "Discover applications via subaccount/instance destinations carrying app_host_id + sap.cloud.service, instead of via service instance keys")
	html5AppsCmd.Flags().String("no-key-option", "", "Apply one choice to every html5-apps-repo instance found with no service key, instead of prompting per instance: s/skip, c/create (auto-create one), or a/abort (stop the command); omit for the interactive prompt")
	html5AppsCmd.Flags().Bool("no-prompt", false, "Skip interactive destination-resolution prompts under --from-destinations — warn and skip instead of prompting to create a missing destination service key")
}
