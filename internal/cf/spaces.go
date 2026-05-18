package cf

import (
	"context"
	"fmt"
)

type Space struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
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
