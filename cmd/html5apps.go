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

// html5AppRow mirrors every field GET .../applications/metadata/ is
// observed to return (see html5apps.Application), plus region (not part of
// that response) and the destination_* columns (--from-destinations only).
type html5AppRow struct {
	Region string `json:"region" toon:"region"`

	ApplicationID      string `json:"application_id"      toon:"application_id"`
	ApplicationName    string `json:"application_name"    toon:"application_name"`
	ApplicationVersion string `json:"application_version" toon:"application_version"`
	ApplicationType    string `json:"application_type"    toon:"application_type"`
	SapCloudService    string `json:"sap_cloud_service"   toon:"sap_cloud_service"`
	UITechnology       string `json:"ui_technology"        toon:"ui_technology"`
	Public             bool   `json:"public"               toon:"public"`
	IsDefault          bool   `json:"is_default"           toon:"is_default"`
	ChangedOn          string `json:"changed_on"           toon:"changed_on"`
	CreatedOn          string `json:"created_on"           toon:"created_on"`

	OrganizationID   string `json:"organization_id"   toon:"organization_id"`
	OrganizationName string `json:"organization_name" toon:"organization_name"`
	SpaceID          string `json:"space_id"          toon:"space_id"`
	SpaceName        string `json:"space_name"        toon:"space_name"`
	SubaccountID     string `json:"subaccount_id"     toon:"subaccount_id"`
	ZoneID           string `json:"zone_id"           toon:"zone_id"`
	Subdomain        string `json:"subdomain"         toon:"subdomain"`
	GlobalAccountID  string `json:"global_account_id" toon:"global_account_id"`

	InstanceName    string `json:"instance_name"    toon:"instance_name"`
	AppHostID       string `json:"app_host_id"      toon:"app_host_id"`
	DeployID        string `json:"deploy_id"        toon:"deploy_id"`
	ContentProvider bool   `json:"content_provider" toon:"content_provider"`

	// The following three are only populated with --from-destinations: the
	// destination record itself (DestinationName) and, when it was found at
	// instance level, the destination service instance that hosts it.
	// DestinationInstanceName/ID are left empty for a subaccount-level
	// destination, since there is no single instance to attribute it to.
	DestinationInstanceName string `json:"destination_instance_name,omitempty" toon:"destination_instance_name,omitempty"`
	DestinationInstanceID   string `json:"destination_instance_id,omitempty"   toon:"destination_instance_id,omitempty"`
	DestinationName         string `json:"destination_name,omitempty"          toon:"destination_name,omitempty"`
}

// html5RowFromApplication copies every field the applications/metadata API
// returned for one application into an output row, adding the region (not
// part of the API response) and, under --from-destinations, the
// destination-identifying columns (empty otherwise).
func html5RowFromApplication(a html5apps.Application, region, destInstanceName, destInstanceID, destName string) html5AppRow {
	return html5AppRow{
		Region:             region,
		ApplicationID:      a.ApplicationID,
		ApplicationName:    a.ApplicationName,
		ApplicationVersion: a.ApplicationVersion,
		ApplicationType:    a.ApplicationType,
		SapCloudService:    a.SapCloudService,
		UITechnology:       a.UITechnology,
		Public:             a.Public,
		IsDefault:          a.IsDefault,
		ChangedOn:          a.ChangedOn,
		CreatedOn:          a.CreatedOn,
		OrganizationID:     a.OrganizationID,
		OrganizationName:   a.OrganizationName,
		SpaceID:            a.SpaceID,
		SpaceName:          a.SpaceName,
		SubaccountID:       a.SubaccountID,
		ZoneID:             a.ZoneID,
		Subdomain:          a.Subdomain,
		GlobalAccountID:    a.GlobalAccountID,
		InstanceName:       a.InstanceName,
		AppHostID:          a.AppHostID,
		DeployID:           a.DeployID,
		ContentProvider:    a.ContentProvider,

		DestinationInstanceName: destInstanceName,
		DestinationInstanceID:   destInstanceID,
		DestinationName:         destName,
	}
}

type html5AppsDoc struct {
	Apps []html5AppRow `json:"apps" toon:"apps"`
}

// ── service-instance based discovery ────────────────────────────────────────

// html5RegionResult holds one region's orgs/spaces plus the
// html5-apps-repo/app-runtime and /app-host plan GUIDs found in that region
// (empty if a plan isn't offered there).
type html5RegionResult struct {
	Region             string
	Client             *cf.Client
	Orgs               []cf.Organization
	Spaces             []cf.Space
	AppRuntimePlanGUID string
	AppHostPlanGUID    string
	Err                error
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

			var appRuntimePlanGUID string
			if plan, planErr := client.FindServicePlan(ctx, "html5-apps-repo", "app-runtime"); planErr != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] looking up html5-apps-repo/app-runtime plan: %v\n", regionName, planErr)
			} else if plan != nil {
				appRuntimePlanGUID = plan.GUID
			}
			var appHostPlanGUID string
			if plan, planErr := client.FindServicePlan(ctx, "html5-apps-repo", "app-host"); planErr != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] looking up html5-apps-repo/app-host plan: %v\n", regionName, planErr)
			} else if plan != nil {
				appHostPlanGUID = plan.GUID
			}

			results[idx] = html5RegionResult{
				Region: regionName, Client: client,
				Orgs: filteredOrgs, Spaces: spaces,
				AppRuntimePlanGUID: appRuntimePlanGUID, AppHostPlanGUID: appHostPlanGUID,
			}
		}(i, apiURL)
	}
	wg.Wait()
	return results
}

// html5AutoKeyName is the service key name used by --no-key-option/
// --no-runtime-option's automatic creation of a service key.
const html5AutoKeyName = "bo-html5-key"

// html5AutoRuntimeName is the service instance name used by
// --no-runtime-option=c/t's automatic creation of an html5-apps-repo/
// app-runtime service instance.
const html5AutoRuntimeName = "bo-html5-app-runtime"

// html5KeyOption is the normalized value of --no-key-option, governing what
// happens for every html5-apps-repo/app-runtime instance found with no
// service key during one run (see normalizeHTML5KeyOption). html5KeyOptionPrompt
// is never a valid flag value — it's assigned internally (see the RunE
// below) when --no-key-option was omitted and -y wasn't given, so each such
// instance is asked about interactively instead of defaulting straight to
// skip.
type html5KeyOption string

const (
	html5KeyOptionPrompt    html5KeyOption = "prompt"    // ask interactively, per instance (default when --no-key-option is omitted and -y isn't given)
	html5KeyOptionSkip      html5KeyOption = "skip"      // warn and skip (default when -y is given without --no-key-option)
	html5KeyOptionCreate    html5KeyOption = "create"    // create a service key automatically, and keep it
	html5KeyOptionTransient html5KeyOption = "transient" // create a service key, use it, then delete it
	html5KeyOptionAbort     html5KeyOption = "abort"     // stop the whole command immediately
)

// normalizeHTML5KeyOption validates and normalizes --no-key-option's raw
// value (accepting both the short and long form of each option). Never
// returns html5KeyOptionPrompt — that's only assigned internally when the
// flag is omitted entirely.
func normalizeHTML5KeyOption(raw string) (html5KeyOption, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "s", "skip":
		return html5KeyOptionSkip, nil
	case "c", "create":
		return html5KeyOptionCreate, nil
	case "t", "transient":
		return html5KeyOptionTransient, nil
	case "a", "abort":
		return html5KeyOptionAbort, nil
	default:
		return "", fmt.Errorf("invalid --no-key-option %q: must be one of s/skip, c/create, t/transient, a/abort", raw)
	}
}

// html5RuntimeOption is the normalized value of --no-runtime-option,
// governing what happens when a space has no html5-apps-repo/app-runtime
// service instance at all (as opposed to --no-key-option, which governs an
// app-runtime instance that exists but has no service key). As with
// html5KeyOptionPrompt above, html5RuntimeOptionPrompt is assigned
// internally, never parsed from the flag's own value.
type html5RuntimeOption string

const (
	html5RuntimeOptionPrompt    html5RuntimeOption = "prompt"    // ask interactively, per space (default when --no-runtime-option is omitted and -y isn't given)
	html5RuntimeOptionSkip      html5RuntimeOption = "skip"      // warn and skip the space's app-hosts (default when -y is given without --no-runtime-option)
	html5RuntimeOptionCreate    html5RuntimeOption = "create"    // create an app-runtime instance + service key, and keep them
	html5RuntimeOptionTransient html5RuntimeOption = "transient" // create an app-runtime instance + service key, use them, then delete both
	html5RuntimeOptionAbort     html5RuntimeOption = "abort"     // stop the whole command immediately
)

// normalizeHTML5RuntimeOption validates and normalizes --no-runtime-option's
// raw value (accepting both the short and long form of each option). Never
// returns html5RuntimeOptionPrompt — see html5RuntimeOption's doc comment.
func normalizeHTML5RuntimeOption(raw string) (html5RuntimeOption, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "s", "skip":
		return html5RuntimeOptionSkip, nil
	case "c", "create":
		return html5RuntimeOptionCreate, nil
	case "t", "transient":
		return html5RuntimeOptionTransient, nil
	case "a", "abort":
		return html5RuntimeOptionAbort, nil
	default:
		return "", fmt.Errorf("invalid --no-runtime-option %q: must be one of s/skip, c/create, t/transient, a/abort", raw)
	}
}

// promptHTML5KeyOption asks what to do about one html5-apps-repo/app-runtime
// instance found with no service key, when --no-key-option wasn't given (and
// -y wasn't either): s/skip (default), c/create, t/transient
// (create-then-delete), or a/abort. Loops on unrecognized input; a
// closed/cancelled stdin is treated as abort, the same convention
// promptUpdateCreateAbort uses.
func promptHTML5KeyOption(ctx context.Context, out io.Writer, instName, instGUID string) html5KeyOption {
	for {
		fmt.Fprintf(out, "\nNo service key found for html5-apps-repo/app-runtime instance %q (%s).\n"+
			"  s/skip (default), c/create, t/transient (create, use, then delete), or a/abort? ",
			instName, instGUID)
		text, ok := readLine(ctx)
		if !ok {
			return html5KeyOptionAbort
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "", "s", "skip":
			return html5KeyOptionSkip
		case "c", "create":
			return html5KeyOptionCreate
		case "t", "transient":
			return html5KeyOptionTransient
		case "a", "abort":
			return html5KeyOptionAbort
		default:
			fmt.Fprintln(out, "Please answer s, c, t, or a.")
		}
	}
}

// promptHTML5RuntimeOption is promptHTML5KeyOption's counterpart for a space
// with no html5-apps-repo/app-runtime service instance at all.
func promptHTML5RuntimeOption(ctx context.Context, out io.Writer, region, spaceName string) html5RuntimeOption {
	for {
		fmt.Fprintf(out, "\nNo html5-apps-repo/app-runtime service instance found in space %q [%s].\n"+
			"  s/skip (default), c/create, t/transient (create, use, then delete), or a/abort? ",
			spaceName, region)
		text, ok := readLine(ctx)
		if !ok {
			return html5RuntimeOptionAbort
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "", "s", "skip":
			return html5RuntimeOptionSkip
		case "c", "create":
			return html5RuntimeOptionCreate
		case "t", "transient":
			return html5RuntimeOptionTransient
		case "a", "abort":
			return html5RuntimeOptionAbort
		default:
			fmt.Fprintln(out, "Please answer s, c, t, or a.")
		}
	}
}

// resolveHTML5InstanceToken obtains (or reuses a cached) OAuth token for one
// html5-apps-repo/app-runtime service instance, following the same
// find-service-key / fetch-credentials / get-token flow as
// resolveSpaceDestClients, cached under creds.SpaceHTML5Services. Returns
// ok=false (with a warning already printed) when the instance has to be
// skipped, or a non-nil error if the whole command must stop (chose, or
// interactively answered, abort).
//
// The returned cleanup is non-nil only under --no-key-option=transient,
// when this call itself created the service key being returned: the caller
// must call it exactly once, after it is done using the returned client, to
// delete that temporary key (and drop it from the on-disk cache, since it
// won't exist for a later run to reuse).
func resolveHTML5InstanceToken(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, spaceGUID string, inst cf.ServiceInstance, creds *store.Credentials, keyOption html5KeyOption) (sdDestClient, bool, func(), error) {
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

	var createdBindingGUID string // set only when this call created the key under transient

	if needToken {
		key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
		if keyErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: finding service key for %q: %v\n", inst.Name, keyErr)
			return sdDestClient{}, false, nil, nil
		}
		if key == nil {
			effective := keyOption
			if effective == html5KeyOptionPrompt {
				effective = promptHTML5KeyOption(ctx, cmd.ErrOrStderr(), inst.Name, inst.GUID)
			}
			switch effective {
			case html5KeyOptionAbort:
				return sdDestClient{}, false, nil, fmt.Errorf(
					"no service key for html5-apps-repo instance %q (%s) — aborting",
					inst.Name, inst.GUID)

			case html5KeyOptionCreate, html5KeyOptionTransient:
				fmt.Fprintf(cmd.ErrOrStderr(), "creating service key %q for html5-apps-repo instance %q...\n", html5AutoKeyName, inst.Name)
				bindingGUID, createErr := cfClient.CreateServiceCredentialBinding(ctx, html5AutoKeyName, inst.GUID)
				if createErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: creating service key for %q: %v — skipping\n", inst.Name, createErr)
					return sdDestClient{}, false, nil, nil
				}
				key = &cf.ServiceCredentialBinding{GUID: bindingGUID, Name: html5AutoKeyName}
				if effective == html5KeyOptionTransient {
					createdBindingGUID = bindingGUID
				}

			default: // html5KeyOptionSkip
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: no service key for html5-apps-repo/app-runtime instance %q (%s) — skipping (pass --no-key-option=c/t to create one automatically)\n",
					inst.Name, inst.GUID)
				return sdDestClient{}, false, nil, nil
			}
		}

		details, detailErr := cfClient.GetServiceCredentialDetails(ctx, key.GUID)
		if detailErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: fetching key details for %q: %v\n", inst.Name, detailErr)
			return sdDestClient{}, false, nil, nil
		}
		clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
		if missing != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: incomplete credentials in service key for %q (missing %q)\n", inst.Name, missing)
			return sdDestClient{}, false, nil, nil
		}

		newToken, expiry, tokErr := html5apps.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
		if tokErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: token for html5-apps-repo instance %q: %v\n", inst.Name, tokErr)
			return sdDestClient{}, false, nil, nil
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
		return sdDestClient{}, false, nil, nil
	}
	client := sdDestClient{
		InstanceGUID: inst.GUID,
		InstanceName: cached.InstanceName,
		URI:          cached.URI,
		Token:        cached.AccessToken,
	}

	var cleanup func()
	if createdBindingGUID != "" {
		instName, bindingGUID := inst.Name, createdBindingGUID
		cleanup = func() {
			fmt.Fprintf(cmd.ErrOrStderr(), "deleting temporary service key %q for html5-apps-repo instance %q...\n", html5AutoKeyName, instName)
			if delErr := cfClient.DeleteServiceCredentialBinding(ctx, bindingGUID); delErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: deleting temporary service key for %q: %v\n", instName, delErr)
			}
			delete(spaceCache, inst.GUID)
		}
	}
	return client, true, cleanup, nil
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

// html5RowsFromAppHost fetches application metadata for one app-host plan
// service instance (appHost), via an already-resolved app-runtime instance's
// URI/token (rtClient) and the app-host's own GUID passed as the
// "x-app-host-id" header — see html5apps.ListApplicationsForAppHost. This
// mirrors the SAP/cf-html5-apps-repo-cli-plugin's "cf html5-list": app-host
// and app-runtime instances are never bound together, only correlated at
// request time via that header, with the app-host instance's own CF GUID
// used directly as its app_host_id (no separate lookup).
func html5RowsFromAppHost(cmd *cobra.Command, ctx context.Context, rtClient sdDestClient, appHost cf.ServiceInstance, region, orgName, spaceName string) []html5AppRow {
	apps, err := html5apps.ListApplicationsForAppHost(ctx, rtClient.URI, rtClient.Token, appHost.GUID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: app-host %q: listing applications metadata: %v\n",
			region, orgName, spaceName, appHost.Name, err)
		return nil
	}
	rows := make([]html5AppRow, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, html5RowFromApplication(a, region, "", "", ""))
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
func fetchHTML5ViaServiceInstances(ctx context.Context, cmd *cobra.Command, creds *store.Credentials, apiURLs []string, orgGUID string, includeOrgs, excludeOrgs cosOrgSet, spacesFilter map[string]bool, keyOption html5KeyOption, runtimeOption html5RuntimeOption) ([]html5AppRow, error) {
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
		if rr.AppRuntimePlanGUID == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] html5-apps-repo/app-runtime service plan not found — skipping region\n", rr.Region)
			continue
		}
		if rr.AppHostPlanGUID == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] html5-apps-repo/app-host service plan not found — skipping region\n", rr.Region)
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

			appHosts, err := rr.Client.ListServiceInstancesInSpace(ctx, sp.GUID, rr.AppHostPlanGUID)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: listing html5-apps-repo/app-host instances: %v\n",
					rr.Region, spOrgName, sp.Name, err)
				continue
			}
			if len(appHosts) == 0 {
				continue
			}

			runtimeInstances, err := rr.Client.ListServiceInstancesInSpace(ctx, sp.GUID, rr.AppRuntimePlanGUID)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: listing html5-apps-repo/app-runtime instances: %v\n",
					rr.Region, spOrgName, sp.Name, err)
				continue
			}

			// Resolve one app-runtime token for the whole space: per the
			// SAP CLI plugin, any single app-runtime key's URI/token is used
			// to query every app-host instance in the space (the app-host
			// GUID alone, via the x-app-host-id header, scopes the result —
			// see html5RowsFromAppHost), so there's no need for a
			// separate token per app-host.
			rtClient, haveRTClient, rtCleanup, rtErr := resolveAppRuntimeClient(ctx, cmd, rr.Client, rr.Region, sp.GUID, sp.Name, runtimeInstances, rr.AppRuntimePlanGUID, creds, keyOption, runtimeOption)
			if rtErr != nil {
				saveHTML5Cache(cmd, creds)
				return rows, rtErr
			}
			if !haveRTClient {
				if len(runtimeInstances) > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s/%s: no usable html5-apps-repo/app-runtime service key — skipping %d app-host instance(s)\n",
						rr.Region, spOrgName, sp.Name, len(appHosts))
				}
				continue
			}

			for _, appHost := range appHosts {
				rows = append(rows, html5RowsFromAppHost(cmd, ctx, rtClient, appHost, rr.Region, spOrgName, sp.Name)...)
			}
			if rtCleanup != nil {
				rtCleanup()
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
// uses — and returns its region, identity, a client for it, and its spaces.
func findOrgAndSpaces(ctx context.Context, cmd *cobra.Command, orgFlag string, creds *store.Credentials, apiURLs []string) (region, orgGUID, orgName string, cfClient *cf.Client, spaces []cf.Space, err error) {
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
					return store.APIURLToRegion(apiURL), org.GUID, org.Name, c, nil, fmt.Errorf("listing spaces in org %q: %w", org.Name, spErr)
				}
				return store.APIURLToRegion(apiURL), org.GUID, org.Name, c, sp, nil
			}
		}
	}
	return "", "", "", nil, nil, fmt.Errorf("org %q not found in any accessible region", orgFlag)
}

// resolveAppRuntimeClient tries each of instances (html5-apps-repo/
// app-runtime service instances already found in a space) in turn for a
// usable service key (--no-key-option governs an instance with no key — see
// resolveHTML5InstanceToken), returning the first one that works. If
// instances is empty — no app-runtime service instance exists in the space
// at all — --no-runtime-option governs what happens instead (see
// resolveAppRuntimeInstanceNotFound).
//
// A non-nil error means the whole command must stop (chose, or interactively
// answered, abort). The returned cleanup, when non-nil, must be called
// exactly once by the caller after it is done using the returned client, to
// delete whatever this call created only for this one use (a service key
// and/or service instance, under --no-key-option=transient or
// --no-runtime-option=transient).
func resolveAppRuntimeClient(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, region, spaceGUID, spaceName string, instances []cf.ServiceInstance, planGUID string, creds *store.Credentials, keyOption html5KeyOption, runtimeOption html5RuntimeOption) (sdDestClient, bool, func(), error) {
	if len(instances) == 0 {
		return resolveAppRuntimeInstanceNotFound(ctx, cmd, cfClient, region, spaceGUID, spaceName, planGUID, creds, keyOption, runtimeOption)
	}
	for _, inst := range instances {
		c, ok, cleanup, resolveErr := resolveHTML5InstanceToken(ctx, cmd, cfClient, spaceGUID, inst, creds, keyOption)
		if resolveErr != nil {
			return sdDestClient{}, false, nil, resolveErr
		}
		if ok {
			return c, true, cleanup, nil
		}
	}
	return sdDestClient{}, false, nil, nil
}

// resolveAppRuntimeInstanceNotFound applies --no-runtime-option when a space
// has no html5-apps-repo/app-runtime service instance at all: skip warns and
// moves on (default); create provisions a new instance (named
// html5AutoRuntimeName) plus a service key on it, keeping both; transient
// does the same but deletes both again once the caller is done with them
// (via the returned cleanup); abort stops the whole command. If
// runtimeOption is html5RuntimeOptionPrompt, the user is asked interactively
// (promptHTML5RuntimeOption) which of the above to do for this space.
func resolveAppRuntimeInstanceNotFound(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, region, spaceGUID, spaceName, planGUID string, creds *store.Credentials, keyOption html5KeyOption, runtimeOption html5RuntimeOption) (sdDestClient, bool, func(), error) {
	effective := runtimeOption
	if effective == html5RuntimeOptionPrompt {
		effective = promptHTML5RuntimeOption(ctx, cmd.ErrOrStderr(), region, spaceName)
	}
	switch effective {
	case html5RuntimeOptionAbort:
		return sdDestClient{}, false, nil, fmt.Errorf(
			"no html5-apps-repo/app-runtime service instance in space %q (%s) — aborting",
			spaceName, spaceGUID)

	case html5RuntimeOptionCreate, html5RuntimeOptionTransient:
		fmt.Fprintf(cmd.ErrOrStderr(), "creating html5-apps-repo/app-runtime service instance %q in space %q...\n", html5AutoRuntimeName, spaceName)
		instGUID, createErr := cfClient.CreateServiceInstance(ctx, html5AutoRuntimeName, spaceGUID, planGUID)
		if createErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s: creating html5-apps-repo/app-runtime service instance: %v — skipping\n", region, spaceName, createErr)
			return sdDestClient{}, false, nil, nil
		}

		// A fresh instance has no service key yet; always create one for it
		// (an instance we just created ourselves is always safe to issue a
		// key for, regardless of --no-key-option), deleting it together with
		// the instance afterward under transient.
		instKeyOption := html5KeyOptionCreate
		if effective == html5RuntimeOptionTransient {
			instKeyOption = html5KeyOptionTransient
		}
		inst := cf.ServiceInstance{GUID: instGUID, Name: html5AutoRuntimeName}
		c, ok, keyCleanup, resolveErr := resolveHTML5InstanceToken(ctx, cmd, cfClient, spaceGUID, inst, creds, instKeyOption)
		if resolveErr != nil {
			return sdDestClient{}, false, nil, resolveErr
		}
		if !ok {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s: created html5-apps-repo/app-runtime service instance %q but could not obtain a service key for it\n", region, spaceName, html5AutoRuntimeName)
			if effective == html5RuntimeOptionTransient {
				if delErr := cfClient.DeleteServiceInstance(ctx, instGUID); delErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: deleting html5-apps-repo/app-runtime service instance %q: %v\n", html5AutoRuntimeName, delErr)
				}
			}
			return sdDestClient{}, false, nil, nil
		}

		if effective != html5RuntimeOptionTransient {
			return c, true, keyCleanup, nil
		}
		cleanup := func() {
			if keyCleanup != nil {
				keyCleanup()
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "deleting temporary html5-apps-repo/app-runtime service instance %q in space %q...\n", html5AutoRuntimeName, spaceName)
			if delErr := cfClient.DeleteServiceInstance(ctx, instGUID); delErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: deleting html5-apps-repo/app-runtime service instance %q: %v\n", html5AutoRuntimeName, delErr)
			}
		}
		return c, true, cleanup, nil

	default: // html5RuntimeOptionSkip
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: [%s] %s: no html5-apps-repo/app-runtime service instance found — skipping app-host(s) in this space (pass --no-runtime-option=c/t to create one automatically)\n",
			region, spaceName)
		return sdDestClient{}, false, nil, nil
	}
}

// resolveAppRuntimeClientForSpace finds a usable html5-apps-repo/app-runtime
// client within spaceGUID: it looks up the app-runtime plan and lists its
// service instances in the space itself, then delegates to
// resolveAppRuntimeClient.
func resolveAppRuntimeClientForSpace(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, region, spaceGUID, spaceName string, creds *store.Credentials, keyOption html5KeyOption, runtimeOption html5RuntimeOption) (sdDestClient, bool, func(), error) {
	plan, planErr := cfClient.FindServicePlan(ctx, "html5-apps-repo", "app-runtime")
	if planErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s: looking up html5-apps-repo/app-runtime plan: %v\n", region, spaceName, planErr)
		return sdDestClient{}, false, nil, nil
	}
	if plan == nil {
		return sdDestClient{}, false, nil, nil
	}
	instances, err := cfClient.ListServiceInstancesInSpace(ctx, spaceGUID, plan.GUID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %s: listing html5-apps-repo/app-runtime instances: %v\n", region, spaceName, err)
		return sdDestClient{}, false, nil, nil
	}
	return resolveAppRuntimeClient(ctx, cmd, cfClient, region, spaceGUID, spaceName, instances, plan.GUID, creds, keyOption, runtimeOption)
}

// html5DestAppHostInfo reports whether dest carries both an "app_host_id"
// and a "sap.cloud.service" property — the marker for a destination
// exposing one app host's content — and if so returns that app_host_id and
// the destination's own "Name".
func html5DestAppHostInfo(dest map[string]string) (appHostID, name string, ok bool) {
	appHostID = destPropCI(dest, "app_host_id")
	cloudService := destPropCI(dest, "sap.cloud.service")
	if appHostID == "" || cloudService == "" {
		return "", "", false
	}
	return appHostID, destPropCI(dest, "Name"), true
}

// html5RowsFromDestAppHost fetches application metadata for one app_host_id
// discovered from a destination (see html5DestAppHostInfo), via an
// already-resolved CF html5-apps-repo/app-runtime client (rtClient) — this
// mirrors the SAP/cf-html5-apps-repo-cli-plugin's "-d/--destination" flag: a
// destination's app_host_id is only ever used to select which app host to
// query; the actual API call always authenticates with a real CF
// app-runtime service key, never with the destination's own credentials.
// destInstanceName/destInstanceID identify the destination service instance
// the destination was found in — pass "" for both when dest is
// subaccount-level.
func html5RowsFromDestAppHost(cmd *cobra.Command, ctx context.Context, rtClient sdDestClient, appHostID, name, region, destInstanceName, destInstanceID string) []html5AppRow {
	apps, err := html5apps.ListApplicationsForAppHost(ctx, rtClient.URI, rtClient.Token, appHostID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: destination %q (app_host_id %s): listing applications metadata: %v\n", name, appHostID, err)
		return nil
	}
	rows := make([]html5AppRow, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, html5RowFromApplication(a, region, destInstanceName, destInstanceID, name))
	}
	return rows
}

// fetchHTML5ViaDestinations is the --from-destinations discovery path: for
// every target org, it inspects both subaccount-level destinations and
// every space's instance-level destinations, extracting app_host_id from
// whichever ones carry an app_host_id/sap.cloud.service pair (see
// html5DestAppHostInfo), then fetches that app host's metadata via a real CF
// html5-apps-repo/app-runtime service key — never the destination's own
// credentials — matching the SAP CLI plugin's own "-d/--destination"
// behavior. The app-runtime client is resolved once per space (reused for
// every destination found there) and once more, lazily, for subaccount-level
// destinations (tried across spacesFilter-matching spaces in the org, since
// a subaccount-level destination isn't tied to one space).
//
// When spacesFilter narrows scope (--spaces given), subaccount-level
// destinations are skipped entirely — they aren't tied to any one space, so
// they'd otherwise pull in app hosts outside the spaces the caller asked
// for; only the matching spaces' instance-level destinations are queried.
func fetchHTML5ViaDestinations(ctx context.Context, cmd *cobra.Command, creds *store.Credentials, apiURLs []string, orgTargets []string, spacesFilter map[string]bool, noPrompt bool, keyOption html5KeyOption, runtimeOption html5RuntimeOption) ([]html5AppRow, error) {
	var rows []html5AppRow
	for _, target := range orgTargets {
		region, _, orgName, cfClient, spaces, err := findOrgAndSpaces(ctx, cmd, target, creds, apiURLs)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			continue
		}

		if spacesFilter == nil {
			// Subaccount-level destinations: resolve an app-runtime client
			// lazily, by trying spaces in the org in turn, and only if a
			// qualifying destination is actually found.
			var orgRTClient sdDestClient
			var orgRTTried, orgRTOK bool
			var orgRTCleanup func()
			resolveOrgRTClient := func() (sdDestClient, bool, error) {
				if orgRTTried {
					return orgRTClient, orgRTOK, nil
				}
				orgRTTried = true
				for _, sp := range spaces {
					c, ok, cleanup, rErr := resolveAppRuntimeClientForSpace(ctx, cmd, cfClient, region, sp.GUID, sp.Name, creds, keyOption, runtimeOption)
					if rErr != nil {
						return sdDestClient{}, false, rErr
					}
					if ok {
						orgRTClient, orgRTOK, orgRTCleanup = c, true, cleanup
						return c, true, nil
					}
				}
				return sdDestClient{}, false, nil
			}

			_, _, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt)
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					saveHTML5Cache(cmd, creds)
					return rows, errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
			} else {
				rawDests, fetchErr := destination.ListSubaccountDestinationsFull(ctx, destClient.URI, destClient.Token)
				if fetchErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing subaccount destinations: %v\n", orgName, fetchErr)
				} else {
					for _, raw := range rawDests {
						appHostID, name, ok := html5DestAppHostInfo(raw)
						if !ok {
							continue
						}
						rtClient, rtOK, rtErr := resolveOrgRTClient()
						if rtErr != nil {
							saveHTML5Cache(cmd, creds)
							return rows, rtErr
						}
						if !rtOK {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] destination %q: no usable html5-apps-repo/app-runtime service key found in any space — skipping\n", orgName, name)
							continue
						}
						rows = append(rows, html5RowsFromDestAppHost(cmd, ctx, rtClient, appHostID, name, region, "", "")...)
					}
				}
			}
			if orgRTCleanup != nil {
				orgRTCleanup()
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
					saveHTML5Cache(cmd, creds)
					return rows, errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] %v\n", orgName, sp.Name, spErr)
				continue
			}

			var spRTClient sdDestClient
			var spRTTried, spRTOK bool
			var spRTCleanup func()

			for _, c := range clients {
				rawDests, fetchErr := destination.ListInstanceDestinationsFull(ctx, c.URI, c.Token)
				if fetchErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] instance %q: listing destinations: %v\n",
						orgName, sp.Name, c.InstanceName, fetchErr)
					continue
				}
				for _, raw := range rawDests {
					appHostID, name, ok := html5DestAppHostInfo(raw)
					if !ok {
						continue
					}
					if !spRTTried {
						spRTTried = true
						rtC, rtOK, cleanup, rtErr := resolveAppRuntimeClientForSpace(ctx, cmd, cfClient, region, sp.GUID, sp.Name, creds, keyOption, runtimeOption)
						if rtErr != nil {
							saveHTML5Cache(cmd, creds)
							return rows, rtErr
						}
						spRTClient, spRTOK, spRTCleanup = rtC, rtOK, cleanup
					}
					if !spRTOK {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] destination %q: no usable html5-apps-repo/app-runtime service key in this space — skipping\n", orgName, sp.Name, name)
						continue
					}
					rows = append(rows, html5RowsFromDestAppHost(cmd, ctx, spRTClient, appHostID, name, region, c.InstanceName, c.InstanceGUID)...)
				}
			}
			if spRTCleanup != nil {
				spRTCleanup()
			}
		}
	}
	saveHTML5Cache(cmd, creds)
	return rows, nil
}

// ── rendering ────────────────────────────────────────────────────────────────

// renderHTML5AppsToon and renderHTML5AppsJSON take val as returned by either
// html5AppsDoc{Apps: rows} (no --fields) or buildFilteredRows wrapped the
// same way (see the RunE below) — both are just Go values that toonenc/json
// reflect over, so narrowing the fields is transparent to them.

func renderHTML5AppsToon(w io.Writer, val interface{}, color bool) error {
	out, err := toonenc.Marshal(val, toonenc.WithIndent(2))
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

func renderHTML5AppsJSON(w io.Writer, val interface{}) error {
	out, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

// renderHTML5AppsCSV writes fieldIDs (fieldIDsOf[html5AppRow]() when empty,
// i.e. every column) as the header, then one row per app.
func renderHTML5AppsCSV(w io.Writer, rows []html5AppRow, color bool, fieldIDs []string) error {
	csvW := newRainbowCSVWriter(w, color)
	defer csvW.Flush()
	return writeFlatCSV(csvW, rows, fieldIDs)
}

// ── command ───────────────────────────────────────────────────────────────────

var html5AppsCmd = &cobra.Command{
	Use:   "html5-apps",
	Short: "List HTML5 application metadata from HTML5 Application Repository service instances",
	Long: `List the applications stored in every accessible HTML5 Application Repository
Service (html5-apps-repo) app-host service instance, across one or more
regions and organizations.

This follows the same discovery flow as the SAP/cf-html5-apps-repo-cli-plugin
CF CLI plugin's "cf html5-list": for each space, every html5-apps-repo/
app-host plan service instance is found (its CF service-instance GUID is
itself the app_host_id the API expects — the two plans are never bound
together, only correlated at request time), and an OAuth2 client_credentials
token is obtained from any one html5-apps-repo/app-runtime plan instance's
service key in that same space (creating one interactively if none exists).
GET {app-runtime uri}/applications/metadata is then called once per app-host
instance, with that instance's GUID passed in the "x-app-host-id" header, to
list its applications.

The result is a flat list, one row per application, with every field the
applications/metadata API itself returns (confirmed via --debug against a
live service), plus region: region, application_id, application_name,
application_version, application_type, sap_cloud_service, ui_technology,
public, is_default, changed_on, created_on, organization_id,
organization_name, space_id, space_name, subaccount_id, zone_id, subdomain,
global_account_id, instance_name, app_host_id, deploy_id, content_provider,
destination_instance_name, destination_instance_id, destination_name. The
destination_* columns are only populated with --from-destinations (see
below).

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows, one per application

Use --fields <column,...> to limit output to just those columns (any of the
ones listed above, case-insensitive, comma-separated) — the order given is
the order they're output in, for every format. Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns, in priority order (ties on the first column are broken by the next
one, and so on) — suffix a column with ":desc" for descending (":asc" is
the default, so it's only useful for clarity), e.g.
--sort-by space_name:desc,application_name. Omit it to keep the default order:
region, organization_name, space_name, instance_name, application_name
(all ascending).

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. Use --spaces to further narrow to specific space names or
GUIDs (comma-separated, case-insensitive).

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and a row matches if organization_name,
space_name, instance_name, destination_instance_name, destination_name,
application_name, or application_version contains any of them
(case-insensitive) — regardless of what --fields selects for the output
itself.

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise
(including no --output) the default toon applies. Pass --format explicitly
to override the extension.

Use --from-destinations to discover applications via destinations instead of
enumerating app-host service instances directly: subaccount-level and
instance-level destinations in the targeted orgs/spaces are scanned for ones
carrying both an "app_host_id" and a "sap.cloud.service" property, and each
such destination's app_host_id is used — exactly like the
SAP/cf-html5-apps-repo-cli-plugin's own "-d/--destination" flag — only to
select which app host to query; the actual metadata call always
authenticates with a real CF html5-apps-repo/app-runtime service key (found
in the same space for an instance-level destination, or in any space in the
org for a subaccount-level one), never with the destination's own
clientId/clientSecret/uri. This also sidesteps destinations that point at a
design-time-only ("-dt") html5-apps-repo host, since the destination's own
"uri" is never called.
When --spaces narrows scope, subaccount-level destinations are
skipped entirely instead — they aren't tied to any one space, so scanning
them would otherwise pull in app hosts outside the spaces asked for; only
those spaces' instance-level destinations are queried.
destination_name is always the destination's own Name; for an
instance-level destination, destination_instance_name/id additionally
identify the destination service instance it was found in, while a
subaccount-level destination leaves both empty (there is no single instance
to attribute it to). instance_name and app_host_id still come straight from
the applications/metadata API response itself in this path too, same as
without --from-destinations.

--no-key-option <s/skip|c/create|t/transient|a/abort> governs what happens
to every html5-apps-repo/app-runtime instance found with no service key
during this run — an instance that exists but has no key: skip warns and
moves on; create runs the equivalent of
'cf create-service-key <instance> bo-html5-key' and keeps the key;
transient does the same but deletes that key again once it's no longer
needed for this run; abort stops the whole command the first time it
happens. Omit this flag to be asked s/skip/c/create/t/transient/a/abort
interactively each time such an instance is found instead, defaulting to
skip on a blank answer — unless -y is also given, in which case skip is
used directly with no prompt. This governs the html5-apps-repo/app-runtime
service-key decision in both discovery paths — --no-prompt below is the
equivalent for the separate destination-resolution prompts encountered
under --from-destinations (reading destinations themselves, not the
app-runtime key used to query them).

--no-runtime-option <s/skip|c/create|t/transient|a/abort> governs what
happens when a space has no html5-apps-repo/app-runtime service instance at
all (as opposed to --no-key-option above, which only applies once an
app-runtime instance exists but lacks a key): skip warns and skips that
space's app-host(s); create provisions a new app-runtime service instance
(named bo-html5-app-runtime) plus a service key on it, and keeps both;
transient does the same but deletes both again once they're no longer
needed for this run; abort stops the whole command the first time it
happens. Omit this flag (same as --no-key-option above) to be asked
interactively each time instead, defaulting to skip — unless -y is given,
in which case skip is used directly. This too governs both discovery paths.

-y skips the interactive s/c/t/a prompt that --no-key-option/
--no-runtime-option would otherwise trigger when either is omitted,
defaulting both straight to skip instead; it has no effect on either flag
when it's explicitly given.

--no-prompt skips interactive destination-resolution prompts encountered
under --from-destinations, warning and skipping instead — useful for
non-interactive/CI runs.

--debug prints each applications/metadata request's raw JSON response body
to stderr before it's parsed — useful to check for any field the API starts
returning beyond the ones this command currently surfaces.

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
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		keyOptionFlag, _ := cmd.Flags().GetString("no-key-option")
		keyOption, err := normalizeHTML5KeyOption(keyOptionFlag)
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("no-key-option") && !skipConfirm {
			keyOption = html5KeyOptionPrompt
		}
		runtimeOptionFlag, _ := cmd.Flags().GetString("no-runtime-option")
		runtimeOption, err := normalizeHTML5RuntimeOption(runtimeOptionFlag)
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("no-runtime-option") && !skipConfirm {
			runtimeOption = html5RuntimeOptionPrompt
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs, err := parseFieldsFlag[html5AppRow](fieldsFlag, "--fields")
		if err != nil {
			return err
		}
		sortFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := parseSortByFlag[html5AppRow](sortFlag, "--sort-by")
		if err != nil {
			return err
		}
		debugFlag, _ := cmd.Flags().GetBool("debug")
		html5apps.Debug = debugFlag

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
			rows, err = fetchHTML5ViaDestinations(ctx, cmd, creds, apiURLs, orgTargets, spacesFilter, noPrompt, keyOption, runtimeOption)
		} else {
			rows, err = fetchHTML5ViaServiceInstances(ctx, cmd, creds, apiURLs, orgGUID, includeOrgs, excludeOrgs, spacesFilter, keyOption, runtimeOption)
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
				r.OrganizationName, r.SpaceName, r.InstanceName,
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

		if len(sortKeys) > 0 {
			sortRowsBy(rows, sortKeys)
		} else {
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Region != rows[j].Region {
					return rows[i].Region < rows[j].Region
				}
				if rows[i].OrganizationName != rows[j].OrganizationName {
					return rows[i].OrganizationName < rows[j].OrganizationName
				}
				if rows[i].SpaceName != rows[j].SpaceName {
					return rows[i].SpaceName < rows[j].SpaceName
				}
				if rows[i].InstanceName != rows[j].InstanceName {
					return rows[i].InstanceName < rows[j].InstanceName
				}
				return rows[i].ApplicationName < rows[j].ApplicationName
			})
		}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		var docVal interface{} = html5AppsDoc{Apps: rows}
		if len(fieldIDs) > 0 {
			docVal = struct {
				Apps interface{} `json:"apps" toon:"apps"`
			}{Apps: buildFilteredRows(rows, fieldIDs)}
		}
		switch strings.ToLower(format) {
		case "json":
			return renderHTML5AppsJSON(out, docVal)
		case "csv":
			return renderHTML5AppsCSV(out, rows, color, fieldIDs)
		default:
			return renderHTML5AppsToon(out, docVal, color)
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
	html5AppsCmd.Flags().String("include", "", "Only include rows where organization_name, space_name, instance_name, destination_instance_name, destination_name, application_name, or application_version contain any of these comma-separated keywords")
	html5AppsCmd.Flags().String("exclude", "", "Exclude rows where organization_name, space_name, instance_name, destination_instance_name, destination_name, application_name, or application_version contain any of these comma-separated keywords")
	html5AppsCmd.Flags().Bool("from-destinations", false, "Discover app_host_id from subaccount/instance destinations carrying app_host_id + sap.cloud.service, then query each via a real html5-apps-repo/app-runtime service key (not the destination's own credentials)")
	html5AppsCmd.Flags().String("no-key-option", "", "What to do with an html5-apps-repo/app-runtime instance found with no service key: s/skip, c/create (auto-create one, kept), t/transient (create, use, then delete it), or a/abort (stop the command); omit to be asked interactively per instance (unless -y), s/skip if -y is given")
	html5AppsCmd.Flags().String("no-runtime-option", "", "What to do when a space has no html5-apps-repo/app-runtime service instance at all: s/skip, c/create (auto-create instance+key, kept), t/transient (create, use, then delete both), or a/abort (stop the command); omit to be asked interactively per space (unless -y), s/skip if -y is given")
	html5AppsCmd.Flags().BoolP("yes", "y", false, "Don't ask interactively about a missing html5-apps-repo/app-runtime service key or instance — default to s/skip instead, unless --no-key-option/--no-runtime-option is also given")
	html5AppsCmd.Flags().Bool("no-prompt", false, "Skip interactive destination-resolution prompts under --from-destinations — warn and skip instead of prompting to create a missing destination service key")
	html5AppsCmd.Flags().String("fields", "", "Comma-separated column IDs to limit output to, in the order given (default: every column; see --help for the full list)")
	html5AppsCmd.Flags().String("sort-by", "", "Comma-separated column IDs to sort by, in priority order, each optionally suffixed with \":desc\" (default: \":asc\"); default sort order is region,organization_name,space_name,instance_name,application_name")
	html5AppsCmd.Flags().Bool("debug", false, "Print each applications/metadata request's raw response body to stderr before parsing")
}
