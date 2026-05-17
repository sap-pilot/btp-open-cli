package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RegionToken holds OAuth tokens for a single CF API endpoint.
type RegionToken struct {
	APIURL       string    `json:"api_url"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Credentials holds tokens for one or more CF API endpoints.
// ActiveAPIURLs records the ordered list from the last login; commands use it
// when no --regions flag is provided. Old tokens for other endpoints are kept
// so the user can switch between region groups without re-logging in.
type Credentials struct {
	ActiveAPIURLs []string               `json:"active_api_urls"`
	Tokens        map[string]RegionToken `json:"tokens"`
}

// RegionToAPIURL converts a region shorthand (e.g. "us10") to the standard
// SAP BTP CF API base URL.
func RegionToAPIURL(region string) string {
	return fmt.Sprintf("https://api.cf.%s.hana.ondemand.com", region)
}

// APIURLToRegion extracts the region identifier from a standard SAP BTP CF
// API URL (e.g. "https://api.cf.us10.hana.ondemand.com" → "us10").
// Returns the full URL unchanged for non-standard patterns.
func APIURLToRegion(apiURL string) string {
	host := strings.TrimPrefix(apiURL, "https://")
	host = strings.TrimPrefix(host, "http://")
	parts := strings.SplitN(host, ".", 4)
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "cf" {
		return parts[2]
	}
	return apiURL
}

func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bo", "credentials.json"), nil
}

func Save(c *Credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if c.Tokens == nil {
		c.Tokens = make(map[string]RegionToken)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func Load() (*Credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Tokens == nil {
		c.Tokens = make(map[string]RegionToken)
	}
	return &c, nil
}

// ClearTokens removes all stored OAuth tokens while preserving ActiveAPIURLs
// so the next login can reuse the same regions without requiring --regions.
func ClearTokens() error {
	creds, err := Load()
	if err != nil {
		// Nothing to clear if the file doesn't exist yet.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	creds.Tokens = make(map[string]RegionToken)
	return Save(creds)
}

func Clear() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
