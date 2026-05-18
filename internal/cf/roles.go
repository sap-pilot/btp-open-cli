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
	Organization struct {
		Data *roleRelationshipData `json:"data"`
	} `json:"organization"`
	Space struct {
		Data *roleRelationshipData `json:"data"`
	} `json:"space"`
}

type Role struct {
	GUID          string            `json:"guid"`
	Type          string            `json:"type"`
	Relationships roleRelationships `json:"relationships"`
}

type rolesResponse struct {
	Pagination pagination `json:"pagination"`
	Resources  []Role     `json:"resources"`
}

// AllRoles holds every role assignment for a CF region indexed by scope GUID.
type AllRoles struct {
	OrgRoles   map[string]map[string][]string // orgGUID   → userGUID → []roleType
	SpaceRoles map[string]map[string][]string // spaceGUID → userGUID → []roleType
}

// ListAllRoles fetches all role assignments for the region in bulk (per_page=5000)
// and returns them indexed by org and space GUID. Call once per region instead
// of once per org/space to avoid hitting API rate limits.
func (c *Client) ListAllRoles(ctx context.Context) (AllRoles, error) {
	all := AllRoles{
		OrgRoles:   make(map[string]map[string][]string),
		SpaceRoles: make(map[string]map[string][]string),
	}
	nextURL := fmt.Sprintf("%s/v3/roles?per_page=5000", c.BaseURL())
	for nextURL != "" {
		var page rolesResponse
		if err := c.get(ctx, nextURL, &page); err != nil {
			return all, err
		}
		for _, r := range page.Resources {
			userGUID := r.Relationships.User.Data.GUID
			if d := r.Relationships.Organization.Data; d != nil {
				m := all.OrgRoles[d.GUID]
				if m == nil {
					m = make(map[string][]string)
					all.OrgRoles[d.GUID] = m
				}
				m[userGUID] = append(m[userGUID], r.Type)
			}
			if d := r.Relationships.Space.Data; d != nil {
				m := all.SpaceRoles[d.GUID]
				if m == nil {
					m = make(map[string][]string)
					all.SpaceRoles[d.GUID] = m
				}
				m[userGUID] = append(m[userGUID], r.Type)
			}
		}
		if page.Pagination.Next != nil {
			nextURL = page.Pagination.Next.Href
		} else {
			nextURL = ""
		}
	}
	return all, nil
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

// CreateOrganizationRole assigns roleType to the user identified by username+origin
// in orgGUID via POST /v3/roles. CF resolves the user from the IdP directly —
// no separate user-creation step is required. Returns nil when already exists (422).
func (c *Client) CreateOrganizationRole(ctx context.Context, roleType, username, origin, orgGUID string) error {
	return c.createRole(ctx, roleType,
		map[string]string{"username": username, "origin": origin},
		"organization", orgGUID,
	)
}

// CreateSpaceRole assigns roleType to the user identified by username+origin
// in spaceGUID via POST /v3/roles. Returns nil when already exists (422).
func (c *Client) CreateSpaceRole(ctx context.Context, roleType, username, origin, spaceGUID string) error {
	return c.createRole(ctx, roleType,
		map[string]string{"username": username, "origin": origin},
		"space", spaceGUID,
	)
}

func (c *Client) createRole(ctx context.Context, roleType string, userData map[string]string, scopeKey, scopeGUID string) error {
	body := map[string]interface{}{
		"type": roleType,
		"relationships": map[string]interface{}{
			"user":    map[string]interface{}{"data": userData},
			scopeKey: map[string]interface{}{"data": map[string]string{"guid": scopeGUID}},
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

// ListSpaceUserRoles returns all role records (with GUIDs) for a specific user
// in a specific space — used when deleting a user's space memberships.
func (c *Client) ListSpaceUserRoles(ctx context.Context, spaceGUID, userGUID string) ([]Role, error) {
	var all []Role
	nextURL := fmt.Sprintf("%s/v3/roles?space_guids=%s&user_guids=%s&per_page=100",
		c.BaseURL(), spaceGUID, userGUID)
	for nextURL != "" {
		var page rolesResponse
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

// ListOrganizationUserRoles returns all role records (with GUIDs) for a specific
// user in a specific org — used when deleting a user's org memberships.
func (c *Client) ListOrganizationUserRoles(ctx context.Context, orgGUID, userGUID string) ([]Role, error) {
	var all []Role
	nextURL := fmt.Sprintf("%s/v3/roles?organization_guids=%s&user_guids=%s&per_page=100",
		c.BaseURL(), orgGUID, userGUID)
	for nextURL != "" {
		var page rolesResponse
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

// DeleteRole removes a single role assignment by its GUID via DELETE /v3/roles/{guid}.
func (c *Client) DeleteRole(ctx context.Context, roleGUID string) error {
	return c.deleteRequest(ctx, fmt.Sprintf("%s/v3/roles/%s", c.BaseURL(), roleGUID))
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
