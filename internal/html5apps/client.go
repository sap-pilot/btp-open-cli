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
	"strings"
	"time"
)

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
// Application Repository runtime API.
type Application struct {
	ApplicationName    string `json:"applicationName"`
	ApplicationVersion string `json:"applicationVersion"`
	ChangedOn          string `json:"changedOn"`
	CreatedOn          string `json:"createdOn"`
	IsDefault          bool   `json:"isDefault"`
	Public             bool   `json:"public"`
}

// ListApplicationsMetadata fetches application metadata from an HTML5
// Application Repository runtime (app-runtime plan) service instance, or
// from a destination pointing at one.
// GET {uri}/applications/metadata/
//
// The trailing slash is deliberate: some backends 301/302 the bare path to
// this one using a broken absolute Location (see httpClient's
// CheckRedirect) — requesting it directly avoids that redirect entirely.
func ListApplicationsMetadata(ctx context.Context, uri, accessToken string) ([]Application, error) {
	u := strings.TrimRight(uri, "/") + "/applications/metadata/"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("html5-apps-repo applications/metadata returned HTTP %d: %s", resp.StatusCode, body)
	}

	var apps []Application
	if err := json.Unmarshal(body, &apps); err != nil {
		return nil, fmt.Errorf("parsing applications metadata response: %w", err)
	}
	return apps, nil
}
