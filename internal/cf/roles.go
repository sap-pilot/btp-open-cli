package cf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// CreateOrganizationRole assigns roleType to userGUID in orgGUID via POST /v3/roles.
// Returns nil when the role already exists (HTTP 422).
func (c *Client) CreateOrganizationRole(ctx context.Context, roleType, userGUID, orgGUID string) error {
	body := map[string]interface{}{
		"type": roleType,
		"relationships": map[string]interface{}{
			"user":         map[string]interface{}{"data": map[string]string{"guid": userGUID}},
			"organization": map[string]interface{}{"data": map[string]string{"guid": orgGUID}},
		},
	}
	err := c.post(ctx, c.BaseURL()+"/v3/roles", body, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
			return nil // role already exists — idempotent
		}
		return err
	}
	return nil
}

// CreateSpaceRole assigns roleType to userGUID in spaceGUID via POST /v3/roles.
// Returns nil when the role already exists (HTTP 422).
func (c *Client) CreateSpaceRole(ctx context.Context, roleType, userGUID, spaceGUID string) error {
	body := map[string]interface{}{
		"type": roleType,
		"relationships": map[string]interface{}{
			"user":  map[string]interface{}{"data": map[string]string{"guid": userGUID}},
			"space": map[string]interface{}{"data": map[string]string{"guid": spaceGUID}},
		},
	}
	err := c.post(ctx, c.BaseURL()+"/v3/roles", body, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
			return nil // role already exists — idempotent
		}
		return err
	}
	return nil
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
