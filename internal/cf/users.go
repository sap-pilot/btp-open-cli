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
	return c.listUsers(ctx, fmt.Sprintf("%s/v3/organizations/%s/users?per_page=100", c.BaseURL(), orgGUID))
}

// ListSpaceUsers fetches all users in a given space, iterating every page.
func (c *Client) ListSpaceUsers(ctx context.Context, spaceGUID string) ([]User, error) {
	return c.listUsers(ctx, fmt.Sprintf("%s/v3/spaces/%s/users?per_page=100", c.BaseURL(), spaceGUID))
}

func (c *Client) listUsers(ctx context.Context, firstURL string) ([]User, error) {
	var all []User
	nextURL := firstURL
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
