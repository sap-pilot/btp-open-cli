// Package html5apps talks to the HTML5 Application Repository Service
// (html5-apps-repo) runtime API, following the same OAuth2 client_credentials
// + REST flow as the SAP/cf-html5-apps-repo-cli-plugin CF CLI plugin.
package html5apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Debug, when set true, makes ListApplicationsForAppHost print the raw
// response body from GET .../applications/metadata/ to stderr before
// unmarshaling it — useful to check for fields the Application struct
// doesn't capture.
var Debug bool

// httpClient pins any redirect it follows to the original request's scheme
// and host. Some html5-apps-repo-rt backends 301/302 a bare
// "/applications/metadata" to "/applications/metadata/" using an absolute
// Location built from their own internal address (http, container port
// 8080) rather than the public https route — that address isn't reachable
// from outside the CF network, so blindly following it just hangs until the
// client timeout. Requesting the trailing-slash path directly (see
// ListApplicationsMetadata) avoids the redirect in the first place; this is
// a second line of defense for any other redirect of the same kind.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		orig := via[0].URL
		if req.URL.Scheme != orig.Scheme || req.URL.Host != orig.Host {
			req.URL.Scheme = orig.Scheme
			req.URL.Host = orig.Host
		}
		return nil
	},
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// GetAccessToken performs an OAuth2 client_credentials flow against
// tokenURL/oauth/token using HTTP Basic auth and returns the token and expiry.
func GetAccessToken(ctx context.Context, tokenURL, clientID, clientSecret string) (string, time.Time, error) {
	endpoint := strings.TrimRight(tokenURL, "/") + "/oauth/token"

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	creds := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("html5-apps-repo token request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("html5-apps-repo token request failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing html5-apps-repo token response: %w", err)
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return tr.AccessToken, expiry, nil
}

// Application is one application's metadata as returned by the HTML5
// Application Repository runtime API — every field GET
// .../applications/metadata/ is observed to return (confirmed via --debug
// against a live service; see ListApplicationsForAppHost).
type Application struct {
	ApplicationID      string `json:"applicationId"`
	ApplicationName    string `json:"applicationName"`
	ApplicationVersion string `json:"applicationVersion"`
	ApplicationType    string `json:"applicationType"`
	SapCloudService    string `json:"sapCloudService"`
	Public             bool   `json:"public"`
	UITechnology       string `json:"uiTechnology"`
	ChangedOn          string `json:"changedOn"`
	CreatedOn          string `json:"createdOn"`
	OrganizationID     string `json:"organizationId"`
	SpaceID            string `json:"spaceId"`
	SubaccountID       string `json:"subaccountId"`
	ZoneID             string `json:"zoneId"`
	Subdomain          string `json:"subdomain"`
	GlobalAccountID    string `json:"globalAccountId"`
	OrganizationName   string `json:"organizationName"`
	SpaceName          string `json:"spaceName"`
	InstanceName       string `json:"instanceName"`
	AppHostID          string `json:"appHostId"`
	DeployID           string `json:"deployId"`
	ContentProvider    bool   `json:"contentProvider"`
	IsDefault          bool   `json:"isDefault"`
}

// ListApplicationsForAppHost fetches application metadata for one specific
// app-host (identified by appHostID — the CF service-instance GUID of an
// html5-apps-repo/app-host plan instance) via an app-runtime plan instance's
// own URI/token, matching the SAP/cf-html5-apps-repo-cli-plugin's
// "ListApplicationsForAppHost" call: the app-runtime key supplies the base
// URL and bearer token, while the app-host GUID is passed in the
// "x-app-host-id" header — the two service instances are never bound to each
// other; they're only correlated at request time via this header.
// GET {uri}/applications/metadata/
//
// The trailing slash is deliberate: some backends 301/302 the bare path to
// this one using a broken absolute Location (see httpClient's
// CheckRedirect) — requesting it directly avoids that redirect entirely.
func ListApplicationsForAppHost(ctx context.Context, uri, accessToken, appHostID string) ([]Application, error) {
	u := strings.TrimRight(uri, "/") + "/applications/metadata/"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-app-host-id", appHostID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if Debug {
		fmt.Fprintf(os.Stderr, "DEBUG: GET %s (x-app-host-id: %s) -> HTTP %d:\n%s\n", u, appHostID, resp.StatusCode, body)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("html5-apps-repo applications/metadata returned HTTP %d: %s", resp.StatusCode, body)
	}

	var apps []Application
	if err := json.Unmarshal(body, &apps); err != nil {
		return nil, fmt.Errorf("parsing applications metadata response: %w", err)
	}
	return apps, nil
}
