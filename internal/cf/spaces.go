package cf

import (
	"context"
	"fmt"
	"strings"
)

type spaceRelationships struct {
	Organization struct {
		Data struct {
			GUID string `json:"guid"`
		} `json:"data"`
	} `json:"organization"`
}

type Space struct {
	GUID          string             `json:"guid"`
	Name          string             `json:"name"`
	Relationships spaceRelationships `json:"relationships"`
}

type spacesResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []Space    `json:"resources"`
}

// ListOrganizationSpaces fetches all spaces in the given org, iterating every page.
func (c *Client) ListOrganizationSpaces(ctx context.Context, orgGUID string) ([]Space, error) {
	var all []Space
	nextURL := fmt.Sprintf("%s/v3/spaces?organization_guids=%s&per_page=5000", c.BaseURL(), orgGUID)

	for nextURL != "" {
		var page spacesResponse
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

// ListSpacesByOrgs fetches all spaces belonging to the given org GUIDs in a
// single batched query, iterating all pages.
func (c *Client) ListSpacesByOrgs(ctx context.Context, orgGUIDs []string) ([]Space, error) {
	var all []Space
	nextURL := fmt.Sprintf("%s/v3/spaces?organization_guids=%s&per_page=5000",
		c.BaseURL(), strings.Join(orgGUIDs, ","))

	for nextURL != "" {
		var page spacesResponse
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

// FindSpaceByGUID looks up a single space by GUID. Returns nil, nil if not found.
func (c *Client) FindSpaceByGUID(ctx context.Context, spaceGUID string) (*Space, error) {
	url := fmt.Sprintf("%s/v3/spaces?guids=%s&per_page=1", c.BaseURL(), spaceGUID)
	var page spacesResponse
	if err := c.get(ctx, url, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	s := page.Resources[0]
	return &s, nil
}

// ListAllSpaces fetches every space in the region with per_page=5000 and
// returns a map of orgGUID → []Space. Call once per region instead of once
// per org to minimise API round trips.
func (c *Client) ListAllSpaces(ctx context.Context) (map[string][]Space, error) {
	byOrg := make(map[string][]Space)
	nextURL := fmt.Sprintf("%s/v3/spaces?per_page=5000", c.BaseURL())

	for nextURL != "" {
		var page spacesResponse
		if err := c.get(ctx, nextURL, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Resources {
			orgGUID := s.Relationships.Organization.Data.GUID
			byOrg[orgGUID] = append(byOrg[orgGUID], s)
		}
		if page.Pagination.Next != nil {
			nextURL = page.Pagination.Next.Href
		} else {
			nextURL = ""
		}
	}
	return byOrg, nil
}
