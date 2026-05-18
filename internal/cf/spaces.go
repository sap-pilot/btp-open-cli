package cf

import (
	"context"
	"fmt"
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
