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
