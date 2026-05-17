package cf

import (
	"context"
	"fmt"
)

type roleRelationshipData struct {
	GUID string `json:"guid"`
}

type roleRelationships struct {
	User struct {
		Data roleRelationshipData `json:"data"`
	} `json:"user"`
}

type Role struct {
	Type          string            `json:"type"`
	Relationships roleRelationships `json:"relationships"`
}

type rolesResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []Role     `json:"resources"`
}

// ListOrganizationRoles fetches all role assignments for the given org and
// returns a map of userGUID → []roleType (e.g. "organization_manager").
func (c *Client) ListOrganizationRoles(ctx context.Context, orgGUID string) (map[string][]string, error) {
	return c.listRoles(ctx, "organization_guids", orgGUID)
}

// ListSpaceRoles fetches all role assignments for the given space and
// returns a map of userGUID → []roleType (e.g. "space_developer").
func (c *Client) ListSpaceRoles(ctx context.Context, spaceGUID string) (map[string][]string, error) {
	return c.listRoles(ctx, "space_guids", spaceGUID)
}

func (c *Client) listRoles(ctx context.Context, filterParam, guid string) (map[string][]string, error) {
	result := make(map[string][]string)
	nextURL := fmt.Sprintf("%s/v3/roles?%s=%s&per_page=100", c.BaseURL(), filterParam, guid)

	for nextURL != "" {
		var page rolesResponse
		if err := c.get(ctx, nextURL, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Resources {
			uid := r.Relationships.User.Data.GUID
			result[uid] = append(result[uid], r.Type)
		}
		if page.Pagination.Next != nil {
			nextURL = page.Pagination.Next.Href
		} else {
			nextURL = ""
		}
	}
	return result, nil
}
