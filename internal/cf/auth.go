package cf

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var authHTTPClient = &http.Client{Timeout: 30 * time.Second}

type cfInfo struct {
	// AuthorizationEndpoint is the login server that issues and validates passcodes.
	// Passcode token exchanges must go here, not to the UAA token_endpoint.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// GetAuthorizationEndpoint returns the login-server base URL (e.g.
// https://login.cf.us10.hana.ondemand.com) by querying the CF API info
// endpoint at cfAPIBaseURL. Passcode URLs and token exchanges must both
// use this endpoint.
func GetAuthorizationEndpoint(ctx context.Context, cfAPIBaseURL string) (string, error) {
	infoURL := strings.TrimRight(cfAPIBaseURL, "/") + "/v2/info"
	req, err := http.NewRequestWithContext(ctx, "GET", infoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching CF API at %s: %w", infoURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CF info endpoint returned HTTP %d", resp.StatusCode)
	}

	var info cfInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("parsing CF info response: %w", err)
	}
	if info.AuthorizationEndpoint == "" {
		return "", fmt.Errorf("CF info response missing authorization_endpoint")
	}
	return info.AuthorizationEndpoint, nil
}

// ExchangePasscode trades a one-time SSO passcode for OAuth tokens.
// authEndpoint must be the login/authorization server (authorization_endpoint
// from /v2/info), not the UAA server, because passcodes are issued and
// validated exclusively by the login server.
func ExchangePasscode(ctx context.Context, authEndpoint, passcode string) (*TokenResponse, error) {
	tokenURL := authEndpoint + "/oauth/token"

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", "passcode")
	form.Set("password", passcode)

	slog.Debug("token exchange request",
		"url", tokenURL,
		"passcode_len", len(passcode),
		"body", "grant_type=password&username=passcode&password=<redacted>",
	)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	// "cf" is a public OAuth client registered in CF UAA with an empty secret.
	creds := base64.StdEncoding.EncodeToString([]byte("cf:"))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting to token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}
	return &tr, nil
}
