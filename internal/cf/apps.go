package cf

import (
	"context"
	"fmt"
	"strings"
)

// ── app types ─────────────────────────────────────────────────────────────────

type AppAnnotations struct {
	MtaID string `json:"mta_id"`
}

type AppMetadata struct {
	Annotations AppAnnotations `json:"annotations"`
}

type appRelationships struct {
	Space struct {
		Data struct {
			GUID string `json:"guid"`
		} `json:"data"`
	} `json:"space"`
}

type App struct {
	GUID          string           `json:"guid"`
	Name          string           `json:"name"`
	State         string           `json:"state"`
	CreatedAt     string           `json:"created_at"`
	UpdatedAt     string           `json:"updated_at"`
	Metadata      AppMetadata      `json:"metadata"`
	Relationships appRelationships `json:"relationships"`
}

type appsResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []App      `json:"resources"`
}

// ── process types ─────────────────────────────────────────────────────────────

type processRelationships struct {
	App struct {
		Data struct {
			GUID string `json:"guid"`
		} `json:"data"`
	} `json:"app"`
}

type Process struct {
	GUID          string               `json:"guid"`
	Instances     int                  `json:"instances"`
	MemoryInMB    int                  `json:"memory_in_mb"`
	DiskInMB      int                  `json:"disk_in_mb"`
	Relationships processRelationships `json:"relationships"`
}

type processesResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []Process  `json:"resources"`
}

// ── queries ───────────────────────────────────────────────────────────────────

// ListAppsBySpaces fetches all apps whose space is in spaceGUIDs, iterating
// all pages returned by the CF v3 API.
func (c *Client) ListAppsBySpaces(ctx context.Context, spaceGUIDs []string) ([]App, error) {
	var all []App
	nextURL := fmt.Sprintf("%s/v3/apps?space_guids=%s&per_page=5000",
		c.BaseURL(), strings.Join(spaceGUIDs, ","))

	for nextURL != "" {
		var page appsResponse
		if err := c.get(ctx, nextURL, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Resources...)
		if page.Pagination.Next != nil {
			nextURL = page.Pagination.Next.Href
		} else {
			nextURL = ""
		}
	}
	return all, nil
}

// ── app environment ────────────────────────────────────────────────────────────

type appEnvResponse struct {
	SystemEnvJSON struct {
		VCAPServices map[string][]map[string]interface{} `json:"VCAP_SERVICES"`
	} `json:"system_env_json"`
}

// GetAppEnv fetches an app's bound service credentials from VCAP_SERVICES
// (part of its system environment variables), grouped by service offering
// exactly as the CF API returns them (e.g. "hana", "xsuaa"). Each binding is
// decoded as a raw map rather than a fixed struct so callers can pass its
// fields through unmodified — VCAP_SERVICES entries vary per service broker
// and "credentials" in particular has no fixed shape across services.
//
// Fetching this requires at least Space Developer access to the app's space;
// callers should treat a failure here as a per-app warning rather than
// aborting a bulk operation across many apps.
func (c *Client) GetAppEnv(ctx context.Context, appGUID string) (map[string][]map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v3/apps/%s/env", c.BaseURL(), appGUID)
	var resp appEnvResponse
	if err := c.get(ctx, url, &resp); err != nil {
		return nil, err
	}
	return resp.SystemEnvJSON.VCAPServices, nil
}

// ListProcessesBySpaces fetches web processes for all apps in spaceGUIDs and
// returns a map of appGUID → Process. Only the "web" process type is fetched.
func (c *Client) ListProcessesBySpaces(ctx context.Context, spaceGUIDs []string) (map[string]Process, error) {
	byApp := make(map[string]Process)
	nextURL := fmt.Sprintf("%s/v3/processes?space_guids=%s&types=web&per_page=5000",
		c.BaseURL(), strings.Join(spaceGUIDs, ","))

	for nextURL != "" {
		var page processesResponse
		if err := c.get(ctx, nextURL, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Resources {
			byApp[p.Relationships.App.Data.GUID] = p
		}
		if page.Pagination.Next != nil {
			nextURL = page.Pagination.Next.Href
		} else {
			nextURL = ""
		}
	}
	return byApp, nil
}
