package cf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// CreateUser creates a user in CF via POST /v3/users. If the user already
// exists (HTTP 422), it falls back to FindUser and returns the existing record.
func (c *Client) CreateUser(ctx context.Context, username, origin string) (*User, error) {
	body := map[string]string{"username": username, "origin": origin}
	var u User
	err := c.post(ctx, c.BaseURL()+"/v3/users", body, &u)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
			return c.FindUser(ctx, username, origin)
		}
		return nil, err
	}
	return &u, nil
}

// FindUser looks up a user by username and origin via GET /v3/users.
func (c *Client) FindUser(ctx context.Context, username, origin string) (*User, error) {
	u := fmt.Sprintf("%s/v3/users?usernames=%s&origins=%s&per_page=1",
		c.BaseURL(), url.QueryEscape(username), url.QueryEscape(origin))
	var page usersResponse
	if err := c.get(ctx, u, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, fmt.Errorf("user %q (origin: %s) not found", username, origin)
	}
	return &page.Resources[0], nil
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
