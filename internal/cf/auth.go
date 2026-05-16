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

type cfInfoRaw struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// Endpoints holds the two OAuth server URLs discovered from /v2/info.
type Endpoints struct {
	// Authorization is the login server (issues/validates SSO passcodes).
	Authorization string
	// Token is the UAA server (accepts direct username/password grants).
	Token string
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// GetEndpoints fetches both OAuth endpoints from the CF API /v2/info.
func GetEndpoints(ctx context.Context, cfAPIBaseURL string) (*Endpoints, error) {
	infoURL := strings.TrimRight(cfAPIBaseURL, "/") + "/v2/info"
	slog.Debug("fetching CF endpoints", "url", infoURL)

	req, err := http.NewRequestWithContext(ctx, "GET", infoURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching CF API at %s: %w", infoURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CF info endpoint returned HTTP %d", resp.StatusCode)
	}

	var raw cfInfoRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing CF info response: %w", err)
	}
	if raw.AuthorizationEndpoint == "" || raw.TokenEndpoint == "" {
		return nil, fmt.Errorf("CF info response missing endpoints (auth=%q token=%q)",
			raw.AuthorizationEndpoint, raw.TokenEndpoint)
	}
	ep := &Endpoints{
		Authorization: raw.AuthorizationEndpoint,
		Token:         raw.TokenEndpoint,
	}
	slog.Debug("CF endpoints resolved", "auth", ep.Authorization, "token", ep.Token)
	return ep, nil
}

// ExchangePasscode trades a one-time SSO passcode for OAuth tokens via the
// login/authorization server. Passcodes are issued and validated there, not
// by the UAA server.
func ExchangePasscode(ctx context.Context, authEndpoint, passcode string) (*TokenResponse, error) {
	return doTokenRequest(ctx, authEndpoint+"/oauth/token",
		url.Values{
			"grant_type": {"password"},
			"username":   {"passcode"},
			"password":   {passcode},
		},
		"passcode exchange",
	)
}

// PasswordLogin authenticates with a username and password directly against
// the UAA server (token_endpoint from /v2/info).
func PasswordLogin(ctx context.Context, tokenEndpoint, username, password string) (*TokenResponse, error) {
	return doTokenRequest(ctx, tokenEndpoint+"/oauth/token",
		url.Values{
			"grant_type": {"password"},
			"username":   {username},
			"password":   {password},
		},
		"password login",
	)
}

// doTokenRequest posts OAuth form data to tokenURL using the public "cf" client.
func doTokenRequest(ctx context.Context, tokenURL string, form url.Values, op string) (*TokenResponse, error) {
	slog.Debug("token request", "op", op, "url", tokenURL)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	// "cf" is the public OAuth client registered in CF UAA with an empty secret.
	creds := base64.StdEncoding.EncodeToString([]byte("cf:"))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: posting to token endpoint: %w", op, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s failed (HTTP %d): %s", op, resp.StatusCode, body)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}
	return &tr, nil
}
