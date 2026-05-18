package cf

import (
	"context"
	"fmt"
)

type pagination struct {
	TotalPages int         `json:"total_pages"`
	Next       *hrefObject `json:"next"`
}

type hrefObject struct {
	Href string `json:"href"`
}

type Organization struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type orgsResponse struct {
	Pagination pagination     `json:"pagination"`
	Resources  []Organization `json:"resources"`
}

// ListOrganizations fetches all organizations the authenticated user has access
// to, iterating every page returned by the CF v3 API.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	var all []Organization
	nextURL := fmt.Sprintf("%s/v3/organizations?per_page=5000", c.BaseURL())

	for nextURL != "" {
		var page orgsResponse
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
