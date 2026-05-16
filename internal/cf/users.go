package cf

import (
	"context"
	"fmt"
)

type User struct {
	GUID     string `json:"guid"`
	Username string `json:"username"`
	Origin   string `json:"origin"`
}

type usersResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []User     `json:"resources"`
}

// ListOrganizationUsers fetches all users in a given org, iterating every page.
func (c *Client) ListOrganizationUsers(ctx context.Context, orgGUID string) ([]User, error) {
	var all []User
	nextURL := fmt.Sprintf("%s/v3/organizations/%s/users?per_page=100", c.BaseURL(), orgGUID)

	for nextURL != "" {
		var page usersResponse
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
