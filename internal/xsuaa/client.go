package xsuaa

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ── types ─────────────────────────────────────────────────────────────────────

type Group struct {
	Value   string `json:"value"`
	Display string `json:"display"`
}

type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

type User struct {
	ID            string  `json:"id"`
	ExternalID    string  `json:"externalId"`
	Origin        string  `json:"origin"`
	UserName      string  `json:"userName"`
	Emails        []Email `json:"emails"`
	LastLogonTime int64   `json:"lastLogonTime"` // milliseconds since epoch
	Groups        []Group `json:"groups"`
}

// PrimaryEmail returns the value of the first email entry, or an empty string.
func PrimaryEmail(emails []Email) string {
	if len(emails) == 0 {
		return ""
	}
	return emails[0].Value
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

// ── HTTP transport, shared clients, and retry ─────────────────────────────────

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if os.Getenv("HTTPS_PROXY_INSECURE") == "true" {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return t
}

// httpClient is used for short-lived auth token requests.
var httpClient = &http.Client{Timeout: 30 * time.Second, Transport: newTransport()}

// scimClient is used for all SCIM API calls (users, groups).
var scimClient = &http.Client{Timeout: 60 * time.Second, Transport: newTransport()}

const (
	maxRetries     = 5
	backoffBase    = 2 * time.Second
	backoffMaxWait = 60 * time.Second
)

// doWithRetry executes makeReq and retries on HTTP 429 (Too Many Requests).
// If the response includes a Retry-After header the delay is taken from it;
// otherwise randomised exponential backoff is used. The context is respected
// during waits so Ctrl-C cancels promptly.
func doWithRetry(ctx context.Context, makeReq func() (*http.Request, error)) (*http.Response, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := makeReq()
		if err != nil {
			return nil, nil, err
		}

		resp, err := scimClient.Do(req)
		if err != nil {
			return nil, nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}

		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRetries {
			return resp, body, nil
		}

		wait := retryAfterDelay(resp.Header.Get("Retry-After"), attempt)
		slog.Warn("XSUAA rate limit hit; retrying", "attempt", attempt+1, "wait", wait.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// retryAfterDelay returns how long to wait before the next attempt.
// If the Retry-After header is present it is respected (seconds or HTTP-date);
// otherwise randomised exponential backoff is applied.
func retryAfterDelay(header string, attempt int) time.Duration {
	if header != "" {
		// Seconds form: "Retry-After: 30"
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		// HTTP-date form: "Retry-After: Wed, 21 Oct 2025 07:28:00 GMT"
		if t, err := http.ParseTime(header); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	// Exponential backoff: base * 2^attempt, capped, plus random jitter in [0, base).
	exp := backoffBase * (1 << attempt)
	if exp > backoffMaxWait {
		exp = backoffMaxWait
	}
	jitter := time.Duration(rand.Int63n(int64(backoffBase)))
	return exp + jitter
}

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
	base := strings.TrimRight(apiBaseURL, "/") + "/Users"

	var all []User
	startIndex := 1
	const pageSize = 500

	for {
		u := fmt.Sprintf("%s?startIndex=%d&count=%d", base, startIndex, pageSize)
		resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Accept", "application/json")
			return req, nil
		})
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
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
	Description       string `json:"description"`
	IsReadOnly        bool   `json:"isReadOnly"`
}

// RoleReference is a role reference inside a role collection.
type RoleReference struct {
	RoleTemplateAppID string `json:"roleTemplateAppId"`
	RoleTemplateName  string `json:"roleTemplateName"`
	Name              string `json:"name"`
	Description       string `json:"description"`
}

// RoleCollection is an XSUAA role collection with its role references.
type RoleCollection struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	IsReadOnly     bool            `json:"isReadOnly"`
	RoleReferences []RoleReference `json:"roleReferences"`
}

// ListRoles fetches all roles from the XSUAA Authorization API.
// The API returns a flat JSON array (no pagination envelope).
func ListRoles(ctx context.Context, apiBaseURL, accessToken string) ([]Role, error) {
	u := strings.TrimRight(apiBaseURL, "/") + "/sap/rest/authorization/v2/roles"

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("XSUAA roles API returned HTTP %d: %s", resp.StatusCode, body)
	}

	var roles []Role
	if err := json.Unmarshal(body, &roles); err != nil {
		return nil, fmt.Errorf("parsing XSUAA roles response: %w", err)
	}
	return roles, nil
}

// ListRoleCollections fetches all role collections from the XSUAA Authorization
// API with showRoles=true. The API returns a flat JSON array (no pagination envelope).
func ListRoleCollections(ctx context.Context, apiBaseURL, accessToken string) ([]RoleCollection, error) {
	u := strings.TrimRight(apiBaseURL, "/") + "/sap/rest/authorization/v2/rolecollections?showRoles=true"

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("XSUAA role collections API returned HTTP %d: %s", resp.StatusCode, body)
	}

	var rcs []RoleCollection
	if err := json.Unmarshal(body, &rcs); err != nil {
		return nil, fmt.Errorf("parsing XSUAA role collections response: %w", err)
	}
	return rcs, nil
}

// CreateUser provisions a new user in the XSUAA tenant via the SCIM API.
// On HTTP 409 (user already exists) it returns (nil, nil) so the caller can
// still proceed to role-collection assignment.
func CreateUser(ctx context.Context, apiBaseURL, accessToken, userName, origin, email string) (*User, error) {
	u := strings.TrimRight(apiBaseURL, "/") + "/Users"

	payload := map[string]interface{}{
		"schemas":  []string{"urn:scim:schemas:core:1.0"},
		"userName": userName,
		"origin":   origin,
		"emails":   []map[string]interface{}{{"value": email, "primary": true}},
		"active":   true,
	}
	b, _ := json.Marshal(payload)

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", u, err)
	}

	if resp.StatusCode == http.StatusConflict {
		return nil, nil // already exists — caller continues to role assignment
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("XSUAA create user failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var created User
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("parsing XSUAA create user response: %w", err)
	}
	return &created, nil
}

// GroupResource is a SCIM group entry as returned by the UAA Groups API.
type GroupResource struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type groupsPage struct {
	TotalResults int             `json:"totalResults"`
	StartIndex   int             `json:"startIndex"`
	ItemsPerPage int             `json:"itemsPerPage"`
	Resources    []GroupResource `json:"Resources"`
}

// ListGroupIDs fetches all groups from the UAA SCIM API and returns a
// displayName→id map (role collection name → group GUID). Call once per org
// and reuse the map for all AddGroupMember calls in that org.
func ListGroupIDs(ctx context.Context, apiBaseURL, accessToken string) (map[string]string, error) {
	base := strings.TrimRight(apiBaseURL, "/") + "/Groups"

	result := make(map[string]string)
	startIndex := 1
	const pageSize = 500

	for {
		u := fmt.Sprintf("%s?startIndex=%d&count=%d", base, startIndex, pageSize)
		resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Accept", "application/json")
			return req, nil
		})
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("UAA Groups API returned HTTP %d: %s", resp.StatusCode, body)
		}

		var page groupsPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parsing UAA Groups response: %w", err)
		}
		for _, g := range page.Resources {
			result[g.DisplayName] = g.ID
		}
		if len(result) >= page.TotalResults || len(page.Resources) == 0 {
			break
		}
		startIndex += len(page.Resources)
	}
	return result, nil
}

// FindUserID returns the XSUAA user ID for a given userName and origin.
// Used to retrieve the ID of an already-existing user (HTTP 409 on create).
func FindUserID(ctx context.Context, apiBaseURL, accessToken, userName, origin string) (string, error) {
	filter := fmt.Sprintf("userName eq %q and origin eq %q", userName, origin)
	u := strings.TrimRight(apiBaseURL, "/") + "/Users?filter=" + url.QueryEscape(filter) + "&count=1"

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("UAA Users filter returned HTTP %d: %s", resp.StatusCode, body)
	}

	var page usersPage
	if err := json.Unmarshal(body, &page); err != nil {
		return "", fmt.Errorf("parsing UAA Users filter response: %w", err)
	}
	if len(page.Resources) == 0 {
		return "", fmt.Errorf("user %q (origin %q) not found", userName, origin)
	}
	return page.Resources[0].ID, nil
}

// FindUserByName returns the first XSUAA user whose userName matches, regardless
// of origin. Used to detect cross-origin conflicts after a 409 on create.
func FindUserByName(ctx context.Context, apiBaseURL, accessToken, userName string) (*User, error) {
	filter := fmt.Sprintf("userName eq %q", userName)
	u := strings.TrimRight(apiBaseURL, "/") + "/Users?filter=" + url.QueryEscape(filter) + "&count=1"

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UAA Users filter returned HTTP %d: %s", resp.StatusCode, body)
	}

	var page usersPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("parsing UAA Users filter response: %w", err)
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	return &page.Resources[0], nil
}

// AddGroupMember adds a user to a SCIM group via POST /Groups/{groupId}/members.
func AddGroupMember(ctx context.Context, apiBaseURL, accessToken, groupID, userID, origin string) error {
	u := strings.TrimRight(apiBaseURL, "/") + "/Groups/" + groupID + "/members"

	payload := map[string]string{"origin": origin, "type": "USER", "value": userID}
	b, _ := json.Marshal(payload)

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("POST %s: %w", u, err)
	}
	// 200 OK or 201 Created both indicate success.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("add member to group %q failed (HTTP %d): %s", groupID, resp.StatusCode, body)
	}
	return nil
}

// DeleteUser sends DELETE /Users/{userID} to remove a user from the XSUAA
// tenant. A 200 or 204 response is treated as success.
func DeleteUser(ctx context.Context, apiBaseURL, accessToken, userID string) error {
	u := strings.TrimRight(apiBaseURL, "/") + "/Users/" + userID

	resp, body, err := doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("XSUAA delete user failed (HTTP %d): %s", resp.StatusCode, body)
	}
	return nil
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
// Prefer ResolveAPIBaseURL when a stored apiurl is available.
func APIBaseURL(region string) string {
	return "https://api.authentication." + region + ".hana.ondemand.com"
}

// ResolveAPIBaseURL returns stored if non-empty (value from the service key's
// "apiurl" field), otherwise falls back to constructing it from region.
func ResolveAPIBaseURL(stored, region string) string {
	if stored != "" {
		return strings.TrimRight(stored, "/")
	}
	return APIBaseURL(region)
}
