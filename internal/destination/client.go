package destination

import (
	"bytes"
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
		return "", time.Time{}, fmt.Errorf("destination token request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("destination token request failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing destination token response: %w", err)
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return tr.AccessToken, expiry, nil
}

// sensitiveDestinationKeys holds lower-cased keys that must be redacted from
// destination properties before returning them to the caller.
var sensitiveDestinationKeys = map[string]bool{
	"password":                   true,
	"proxypassword":              true,
	"clientsecret":               true,
	"tokenservicepassword":       true,
	"tokenserviceclientpassword": true,
}

// BulkResponseItem is one entry in a bulk create/update response from the
// destination service (e.g. POST /v1/instanceDestinations with a JSON array).
type BulkResponseItem struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	ETag   string `json:"etag,omitempty"`
	Cause  string `json:"cause,omitempty"`
}

// instanceDestURL returns the base URL for instance-level destination APIs.
func instanceDestURL(destURI string) string {
	return strings.TrimRight(destURI, "/") + "/destination-configuration/v1/instanceDestinations"
}

// listInstanceDestinations is the shared implementation for the public wrappers.
// When redact is true, sensitive credential fields are omitted from the result.
func listInstanceDestinations(ctx context.Context, destURI, accessToken string, redact bool) ([]map[string]string, error) {
	u := instanceDestURL(destURI)
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
		return nil, fmt.Errorf("destination service returned HTTP %d: %s", resp.StatusCode, body)
	}

	var raw []map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing instance destinations response: %w", err)
	}

	out := make([]map[string]string, 0, len(raw))
	for _, entry := range raw {
		m := make(map[string]string, len(entry))
		for k, v := range entry {
			if redact && sensitiveDestinationKeys[strings.ToLower(k)] {
				continue
			}
			m[k] = fmt.Sprintf("%v", v)
		}
		out = append(out, m)
	}
	return out, nil
}

// ListInstanceDestinations fetches all instance-level destinations.
// GET /destination-configuration/v1/instanceDestinations
// Sensitive credential fields (Password, ClientSecret, etc.) are removed from the response.
func ListInstanceDestinations(ctx context.Context, destURI, accessToken string) ([]map[string]string, error) {
	return listInstanceDestinations(ctx, destURI, accessToken, true)
}

// ListInstanceDestinationsFull fetches all instance-level destinations including
// sensitive credential fields such as Password and ClientSecret.
func ListInstanceDestinationsFull(ctx context.Context, destURI, accessToken string) ([]map[string]string, error) {
	return listInstanceDestinations(ctx, destURI, accessToken, false)
}

// CreateInstanceDestinations posts new destinations to service instance level.
// POST /destination-configuration/v1/instanceDestinations
// body should be a JSON array (or object) matching the destination service schema.
// Returns per-item results when the API provides them (bulk array response),
// or nil for a simple 201 with no body.
func CreateInstanceDestinations(ctx context.Context, destURI, accessToken string, body json.RawMessage) ([]BulkResponseItem, error) {
	u := instanceDestURL(destURI)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("POST instanceDestinations returned HTTP %d: %s", resp.StatusCode, respBody)
	}

	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 || string(trimmed) == `""` {
		return nil, nil
	}
	var items []BulkResponseItem
	_ = json.Unmarshal(trimmed, &items)
	return items, nil
}

// UpdateInstanceDestinations overwrites existing destinations at service instance level.
// PUT /destination-configuration/v1/instanceDestinations
// body should be a JSON array (or object) matching the destination service schema.
func UpdateInstanceDestinations(ctx context.Context, destURI, accessToken string, body json.RawMessage) ([]BulkResponseItem, error) {
	u := instanceDestURL(destURI)
	req, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PUT instanceDestinations returned HTTP %d: %s", resp.StatusCode, respBody)
	}

	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// The API may return either [{name,status,etag}] or {Count:N}.
	// Try array first; fall back gracefully to the scalar form.
	var items []BulkResponseItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, nil // {Count:N} or unknown — treat as success
	}
	return items, nil
}

// deleteCountDeleted parses a destination-service delete response body of the
// form {"Count":"1"} or {"Count":1} and reports whether anything was actually
// removed. Any value > 0 (or unparseable body) is treated as deleted.
func deleteCountDeleted(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return true // 204-style: no body means success
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return true // unrecognised body — assume deleted
	}
	switch v := result["Count"].(type) {
	case float64:
		return v > 0
	case string:
		return v != "" && v != "0"
	}
	return true // field absent or unexpected type — assume deleted
}

// DeleteInstanceDestination deletes a single instance-level destination by name.
// DELETE /destination-configuration/v1/instanceDestinations/{name}
// Returns (true, nil) when the destination was deleted, (false, nil) when it did
// not exist (404 or Count==0), and (false, err) for any other failure.
func DeleteInstanceDestination(ctx context.Context, destURI, accessToken, name string) (bool, error) {
	u := instanceDestURL(destURI) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return deleteCountDeleted(body), nil
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("DELETE instanceDestinations/%s returned HTTP %d: %s", name, resp.StatusCode, body)
	}
}

// subaccountDestURL returns the base URL for subaccount-level destination APIs.
func subaccountDestURL(destURI string) string {
	return strings.TrimRight(destURI, "/") + "/destination-configuration/v1/subaccountDestinations"
}

// listSubaccountDestinations is the shared implementation for the public wrappers.
// When redact is true, sensitive credential fields are omitted from the result.
func listSubaccountDestinations(ctx context.Context, destURI, accessToken string, redact bool) ([]map[string]string, error) {
	u := subaccountDestURL(destURI)
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
		return nil, fmt.Errorf("destination service returned HTTP %d: %s", resp.StatusCode, body)
	}

	var raw []map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing subaccount destinations response: %w", err)
	}

	out := make([]map[string]string, 0, len(raw))
	for _, entry := range raw {
		m := make(map[string]string, len(entry))
		for k, v := range entry {
			if redact && sensitiveDestinationKeys[strings.ToLower(k)] {
				continue
			}
			m[k] = fmt.Sprintf("%v", v)
		}
		out = append(out, m)
	}
	return out, nil
}

// ListSubaccountDestinations fetches all subaccount-level destinations and
// returns them as a slice of property maps with sensitive values removed.
func ListSubaccountDestinations(ctx context.Context, destURI, accessToken string) ([]map[string]string, error) {
	return listSubaccountDestinations(ctx, destURI, accessToken, true)
}

// ListSubaccountDestinationsFull fetches all subaccount-level destinations
// including sensitive credential fields such as Password and ClientSecret.
func ListSubaccountDestinationsFull(ctx context.Context, destURI, accessToken string) ([]map[string]string, error) {
	return listSubaccountDestinations(ctx, destURI, accessToken, false)
}

// CreateSubaccountDestinations posts new destinations to subaccount level.
// POST /destination-configuration/v1/subaccountDestinations
// body should be a JSON array (or object) matching the destination service schema.
// Returns per-item results when the API provides them, or nil for a simple 201.
func CreateSubaccountDestinations(ctx context.Context, destURI, accessToken string, body json.RawMessage) ([]BulkResponseItem, error) {
	u := subaccountDestURL(destURI)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("POST subaccountDestinations returned HTTP %d: %s", resp.StatusCode, respBody)
	}

	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 || string(trimmed) == `""` {
		return nil, nil
	}
	var items []BulkResponseItem
	_ = json.Unmarshal(trimmed, &items)
	return items, nil
}

// UpdateSubaccountDestinations overwrites existing destinations at subaccount level.
// PUT /destination-configuration/v1/subaccountDestinations
// body should be a JSON array (or object) matching the destination service schema.
func UpdateSubaccountDestinations(ctx context.Context, destURI, accessToken string, body json.RawMessage) ([]BulkResponseItem, error) {
	u := subaccountDestURL(destURI)
	req, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PUT subaccountDestinations returned HTTP %d: %s", resp.StatusCode, respBody)
	}

	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var items []BulkResponseItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, nil // {Count:N} or unknown — treat as success
	}
	return items, nil
}

// DeleteSubaccountDestination deletes a single subaccount-level destination by name.
// DELETE /destination-configuration/v1/subaccountDestinations/{name}
// Returns (true, nil) when the destination was deleted, (false, nil) when it did
// not exist (404 or Count==0), and (false, err) for any other failure.
func DeleteSubaccountDestination(ctx context.Context, destURI, accessToken, name string) (bool, error) {
	u := subaccountDestURL(destURI) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return deleteCountDeleted(body), nil
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("DELETE subaccountDestinations/%s returned HTTP %d: %s", name, resp.StatusCode, body)
	}
}
