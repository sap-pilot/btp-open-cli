package cis

import (
	"context"
	"crypto/tls"
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

// Subaccount represents a BTP subaccount returned by the Accounts Service API.
type Subaccount struct {
	GUID              string `json:"guid"`
	DisplayName       string `json:"displayName"`
	GlobalAccountGUID string `json:"globalAccountGUID"`
	Subdomain         string `json:"subdomain"`
	Region            string `json:"region"`
	State             string `json:"state"`
	StateMessage      string `json:"stateMessage"`
	BetaEnabled       bool   `json:"betaEnabled"`
	UsedForProduction string `json:"usedForProduction"`
	Description       string `json:"description"`
	CreatedDate       int64  `json:"createdDate"`  // ms since epoch
	ModifiedDate      int64  `json:"modifiedDate"` // ms since epoch
	ParentGUID        string `json:"parentGUID"`
	ParentType        string `json:"parentType"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if os.Getenv("HTTPS_PROXY_INSECURE") == "true" {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return t
}

var httpClient = &http.Client{Timeout: 30 * time.Second, Transport: newTransport()}

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
		return "", time.Time{}, fmt.Errorf("CIS token request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("CIS token request failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing CIS token response: %w", err)
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return tr.AccessToken, expiry, nil
}

// GetSubaccount fetches subaccount details from the Accounts Service API.
func GetSubaccount(ctx context.Context, accountsServiceURL, accessToken, subaccountGUID string) (*Subaccount, error) {
	u := strings.TrimRight(accountsServiceURL, "/") + "/accounts/v1/subaccounts/" + subaccountGUID
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second, Transport: newTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CIS accounts service returned HTTP %d: %s", resp.StatusCode, body)
	}

	var sa Subaccount
	if err := json.Unmarshal(body, &sa); err != nil {
		return nil, fmt.Errorf("parsing subaccount response: %w", err)
	}
	return &sa, nil
}
