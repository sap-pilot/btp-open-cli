package xsuaa

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

// ── types ─────────────────────────────────────────────────────────────────────

type Group struct {
	Value   string `json:"value"`
	Display string `json:"display"`
}

type User struct {
	ID            string  `json:"id"`
	ExternalID    string  `json:"externalId"`
	Origin        string  `json:"origin"`
	UserName      string  `json:"userName"`
	LastLogonTime int64   `json:"lastLogonTime"` // milliseconds since epoch
	Groups        []Group `json:"groups"`
}

type usersPage struct {
	TotalResults int    `json:"totalResults"`
	StartIndex   int    `json:"startIndex"`
	ItemsPerPage int    `json:"itemsPerPage"`
	Resources    []User `json:"Resources"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// ── HTTP transport (mirrors cf/transport.go to honour proxy env vars) ─────────

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if os.Getenv("HTTPS_PROXY_INSECURE") == "true" {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return t
}

var httpClient = &http.Client{Timeout: 30 * time.Second, Transport: newTransport()}

// ── auth ──────────────────────────────────────────────────────────────────────

// GetAccessToken performs an OAuth2 client_credentials flow against
// xsuaaURL/oauth/token and returns the token string and its expiry time.
func GetAccessToken(ctx context.Context, xsuaaURL, clientID, clientSecret string) (string, time.Time, error) {
	tokenURL := strings.TrimRight(xsuaaURL, "/") + "/oauth/token"

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	creds := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("XSUAA token request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("XSUAA token request failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing XSUAA token response: %w", err)
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return tr.AccessToken, expiry, nil
}

// ── users ─────────────────────────────────────────────────────────────────────

// ListUsers fetches all users from the XSUAA admin API, paginating through
// all pages. apiBaseURL is e.g. "https://api.authentication.us10.hana.ondemand.com".
func ListUsers(ctx context.Context, apiBaseURL, accessToken string) ([]User, error) {
	client := &http.Client{Timeout: 60 * time.Second, Transport: newTransport()}
	base := strings.TrimRight(apiBaseURL, "/") + "/Users"

	var all []User
	startIndex := 1
	const pageSize = 500

	for {
		u := fmt.Sprintf("%s?startIndex=%d&count=%d", base, startIndex, pageSize)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("XSUAA users API returned HTTP %d: %s", resp.StatusCode, body)
		}

		var page usersPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parsing XSUAA users response: %w", err)
		}

		all = append(all, page.Resources...)
		if len(all) >= page.TotalResults || len(page.Resources) == 0 {
			break
		}
		startIndex += len(page.Resources)
	}
	return all, nil
}

// ── roles ─────────────────────────────────────────────────────────────────────

// Role represents a single XSUAA authorization role.
type Role struct {
	RoleTemplateAppID string `json:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"`
	Name              string `json:"name"`
	AppName           string `json:"appName"`
}

// RoleReference is a role reference inside a role collection.
type RoleReference struct {
	RoleTemplateAppID string `json:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"`
	Name              string `json:"name"`
}

// RoleCollection is an XSUAA role collection with its role references.
type RoleCollection struct {
	Name           string          `json:"name"`
	RoleReferences []RoleReference `json:"roleReferences"`
}

type rolesPage struct {
	TotalResults int    `json:"totalResults"`
	StartIndex   int    `json:"startIndex"`
	ItemsPerPage int    `json:"itemsPerPage"`
	Resources    []Role `json:"Resources"`
}

type roleCollectionsPage struct {
	TotalResults int              `json:"totalResults"`
	StartIndex   int              `json:"startIndex"`
	ItemsPerPage int              `json:"itemsPerPage"`
	Resources    []RoleCollection `json:"Resources"`
}

// ListRoles fetches all roles from the XSUAA Authorization API, paginating
// through all pages using startIndex + count=500.
func ListRoles(ctx context.Context, apiBaseURL, accessToken string) ([]Role, error) {
	client := &http.Client{Timeout: 60 * time.Second, Transport: newTransport()}
	base := strings.TrimRight(apiBaseURL, "/") + "/sap/rest/authorization/v2/roles"

	var all []Role
	startIndex := 1
	const pageSize = 500

	for {
		u := fmt.Sprintf("%s?startIndex=%d&count=%d", base, startIndex, pageSize)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("XSUAA roles API returned HTTP %d: %s", resp.StatusCode, body)
		}

		var page rolesPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parsing XSUAA roles response: %w", err)
		}

		all = append(all, page.Resources...)
		if len(all) >= page.TotalResults || len(page.Resources) == 0 {
			break
		}
		startIndex += len(page.Resources)
	}
	return all, nil
}

// ListRoleCollections fetches all role collections from the XSUAA Authorization
// API (with showRoles=true), paginating through all pages.
func ListRoleCollections(ctx context.Context, apiBaseURL, accessToken string) ([]RoleCollection, error) {
	client := &http.Client{Timeout: 60 * time.Second, Transport: newTransport()}
	base := strings.TrimRight(apiBaseURL, "/") + "/sap/rest/authorization/v2/rolecollections"

	var all []RoleCollection
	startIndex := 1
	const pageSize = 500

	for {
		u := fmt.Sprintf("%s?showRoles=true&startIndex=%d&count=%d", base, startIndex, pageSize)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("XSUAA role collections API returned HTTP %d: %s", resp.StatusCode, body)
		}

		var page roleCollectionsPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parsing XSUAA role collections response: %w", err)
		}

		all = append(all, page.Resources...)
		if len(all) >= page.TotalResults || len(page.Resources) == 0 {
			break
		}
		startIndex += len(page.Resources)
	}
	return all, nil
}

// MSToISO converts a Unix timestamp in milliseconds to an ISO 8601 string.
// Returns an empty string for zero values.
func MSToISO(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// GroupValues joins group display values with a semicolon separator.
func GroupValues(groups []Group) string {
	vals := make([]string, len(groups))
	for i, g := range groups {
		vals[i] = g.Value
	}
	return strings.Join(vals, ";")
}

// APIBaseURL returns the XSUAA admin API base URL for a CF region,
// e.g. "us10" → "https://api.authentication.us10.hana.ondemand.com".
func APIBaseURL(region string) string {
	return "https://api.authentication." + region + ".hana.ondemand.com"
}
