package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"
)

const (
	xsuaaServiceOffering = "xsuaa"
	xsuaaServicePlan     = "apiaccess"
)

// xsuaaOrgClient holds a ready-to-use XSUAA access token and admin API URL
// for one CF org.
type xsuaaOrgClient struct {
	OrgGUID    string
	OrgName    string
	RegionName string
	APIURL     string // XSUAA admin API base URL (from service key "apiurl")
	Token      string // valid access token
}

// resolveXsuaaClients scans accessible orgs for any xsuaa/apiaccess service
// instance (across all spaces) that has at least one service key. It uses a
// cached token when still fresh (> 60 s remaining), otherwise fetches fresh
// credentials from CF, obtains a new token, and caches only the token — the
// service key credentials (client ID, secret, token URL) are never persisted.
//
// When no service instance or key is found for an org:
//   - noPrompt=false: prints instructions and prompts the user to create the
//     resource manually, then retries once on Enter; skips on Ctrl-C.
//   - noPrompt=true: prints a warning and skips the org.
func resolveXsuaaClients(
	ctx context.Context,
	apiURLs []string,
	creds *store.Credentials,
	includeOrgs, excludeOrgs cosOrgSet,
	noPrompt bool,
) ([]xsuaaOrgClient, *store.Credentials, error) {
	if creds.OrgXsuaa == nil {
		creds.OrgXsuaa = make(map[string]store.XsuaaData)
	}

	var (
		mu      sync.Mutex // guards creds.OrgXsuaa writes
		clients []xsuaaOrgClient
	)

	for _, apiURL := range apiURLs {
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			regionName := store.APIURLToRegion(apiURL)
			fmt.Fprintf(os.Stderr, "[%s] no token — run: bo login --regions %s\n", regionName, regionName)
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
		slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

		// Fetch the xsuaa/apiaccess plan once per region (lazily).
		var xsuaaPlan *cf.ServicePlan
		planFetched := false

		for _, org := range orgs {
			if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
				continue
			}
			if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
				continue
			}

			// ── use cached token if still fresh ──────────────────────────────
			mu.Lock()
			xd := creds.OrgXsuaa[org.GUID]
			mu.Unlock()

			if xd.AccessToken != "" && !time.Now().Add(60*time.Second).After(xd.TokenExpiry) {
				slog.Debug("using cached XSUAA token", "region", regionName, "org", org.Name)
				clients = append(clients, xsuaaOrgClient{
					OrgGUID:    org.GUID,
					OrgName:    org.Name,
					RegionName: regionName,
					APIURL:     xd.APIURL,
					Token:      xd.AccessToken,
				})
				continue
			}

			// ── fetch fresh credentials from CF ───────────────────────────────
			slog.Debug("resolving XSUAA credentials from CF", "region", regionName, "org", org.Name)

			if !planFetched {
				planFetched = true
				xsuaaPlan, _ = cfClient.FindServicePlan(ctx, xsuaaServiceOffering, xsuaaServicePlan)
			}
			if xsuaaPlan == nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: xsuaa/%s plan not found — skipping\n",
					regionName, org.Name, xsuaaServicePlan)
				continue
			}

			// Find any xsuaa/apiaccess instance in this org (all spaces).
			instances, instErr := cfClient.ListServiceInstancesByPlanGUID(ctx, xsuaaPlan.GUID, org.GUID)
			if instErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: listing xsuaa instances: %v\n", regionName, org.Name, instErr)
				continue
			}

			var inst *cf.ServiceInstance
			if len(instances) > 0 {
				inst = &instances[0]
			}
			if inst == nil {
				inst = xsuaaPromptRetryInstance(ctx, cfClient,
					fmt.Sprintf("[%s] %s: no xsuaa/%s service instance found in any space",
						regionName, org.Name, xsuaaServicePlan),
					org.GUID, xsuaaPlan.GUID, noPrompt)
				if inst == nil {
					continue
				}
			}

			// Find any service key for the instance.
			key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
			if keyErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: looking up service key: %v\n", regionName, org.Name, keyErr)
				continue
			}
			if key == nil {
				key = xsuaaPromptRetryKey(ctx, cfClient,
					fmt.Sprintf("[%s] %s: no service key found for xsuaa instance %q",
						regionName, org.Name, inst.Name),
					inst.GUID, noPrompt)
				if key == nil {
					continue
				}
			}

			// Fetch key credentials (used only to get a token — not stored).
			details, detErr := cfClient.GetServiceCredentialDetails(ctx, key.GUID)
			if detErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: fetching key credentials: %v\n", regionName, org.Name, detErr)
				continue
			}
			clientID, _ := details["clientid"].(string)
			clientSecret, _ := details["clientsecret"].(string)
			xsuaaURL, _ := details["url"].(string)
			xsuaaAPIURL, _ := details["apiurl"].(string)
			if clientID == "" || clientSecret == "" || xsuaaURL == "" {
				fmt.Fprintf(os.Stderr, "[%s] %s: incomplete credentials in service key\n", regionName, org.Name)
				continue
			}

			// Obtain access token — credentials are discarded after this call.
			token, expiry, tokErr := xsuaa.GetAccessToken(ctx, xsuaaURL, clientID, clientSecret)
			if tokErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] %s: XSUAA token: %v\n", regionName, org.Name, tokErr)
				continue
			}

			apiBaseURL := xsuaa.ResolveAPIBaseURL(xsuaaAPIURL, regionName)

			// Persist only the token and API URL (never the client credentials).
			mu.Lock()
			creds.OrgXsuaa[org.GUID] = store.XsuaaData{
				APIURL:      apiBaseURL,
				AccessToken: token,
				TokenExpiry: expiry,
			}
			mu.Unlock()

			clients = append(clients, xsuaaOrgClient{
				OrgGUID:    org.GUID,
				OrgName:    org.Name,
				RegionName: regionName,
				APIURL:     apiBaseURL,
				Token:      token,
			})
		}
	}

	// Persist updated token cache.
	if saveErr := store.Save(creds); saveErr != nil {
		fmt.Fprintf(os.Stderr, "warning: saving credentials: %v\n", saveErr)
	}

	return clients, creds, nil
}

// xsuaaPromptRetryInstance warns that no xsuaa/apiaccess service instance was
// found, optionally prompts the user to create one manually, then retries once.
func xsuaaPromptRetryInstance(
	ctx context.Context,
	cfClient *cf.Client,
	message, orgGUID, planGUID string,
	noPrompt bool,
) *cf.ServiceInstance {
	if noPrompt {
		fmt.Fprintf(os.Stderr, "warning: %s — skipping\n", message)
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"\nWARNING: %s\n"+
			"  Create a service instance in any space, e.g.:\n"+
			"    cf create-service xsuaa apiaccess <instance-name>\n"+
			"  Then press Enter to retry, or Ctrl-C to skip this org.\n",
		message)
	if _, ok := readLine(ctx); !ok {
		return nil
	}
	instances, err := cfClient.ListServiceInstancesByPlanGUID(ctx, planGUID, orgGUID)
	if err != nil || len(instances) == 0 {
		fmt.Fprintf(os.Stderr, "warning: still no xsuaa/%s instance found — skipping\n", xsuaaServicePlan)
		return nil
	}
	return &instances[0]
}

// xsuaaPromptRetryKey warns that no service key was found for an xsuaa instance,
// optionally prompts the user to create one manually, then retries once.
func xsuaaPromptRetryKey(
	ctx context.Context,
	cfClient *cf.Client,
	message, instanceGUID string,
	noPrompt bool,
) *cf.ServiceCredentialBinding {
	if noPrompt {
		fmt.Fprintf(os.Stderr, "warning: %s — skipping\n", message)
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"\nWARNING: %s\n"+
			"  Create a service key, e.g.:\n"+
			"    cf create-service-key <instance-name> <key-name>\n"+
			"  Then press Enter to retry, or Ctrl-C to skip this org.\n",
		message)
	if _, ok := readLine(ctx); !ok {
		return nil
	}
	key, err := cfClient.FindAnyServiceCredentialBinding(ctx, instanceGUID)
	if err != nil || key == nil {
		fmt.Fprintf(os.Stderr, "warning: still no service key found — skipping\n")
		return nil
	}
	return key
}
