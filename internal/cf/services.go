package cf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ── types ─────────────────────────────────────────────────────────────────────

type ServicePlan struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type ServiceInstance struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	LastOperation struct {
		Type  string `json:"type"`
		State string `json:"state"`
	} `json:"last_operation"`
}

type ServiceCredentialBinding struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type serviceCredentialDetails struct {
	Credentials map[string]interface{} `json:"credentials"`
}

type servicePlansResponse struct {
	Pagination pagination    `json:"pagination"`
	Resources  []ServicePlan `json:"resources"`
}

type serviceInstancesResponse struct {
	Pagination pagination        `json:"pagination"`
	Resources  []ServiceInstance `json:"resources"`
}

type serviceCredentialBindingsResponse struct {
	Pagination pagination                 `json:"pagination"`
	Resources  []ServiceCredentialBinding `json:"resources"`
}

// ── service plans ─────────────────────────────────────────────────────────────

// FindServicePlan looks up a service plan by offering name and plan name.
// Returns nil, nil if not found.
func (c *Client) FindServicePlan(ctx context.Context, offeringName, planName string) (*ServicePlan, error) {
	url := fmt.Sprintf("%s/v3/service_plans?service_offering_names=%s&names=%s&per_page=1",
		c.BaseURL(), offeringName, planName)
	var page servicePlansResponse
	if err := c.get(ctx, url, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	return &page.Resources[0], nil
}

// ── service instances ─────────────────────────────────────────────────────────

// FindServiceInstance looks up a managed service instance by name in a space.
// Returns nil, nil if not found.
func (c *Client) FindServiceInstance(ctx context.Context, name, spaceGUID string) (*ServiceInstance, error) {
	url := fmt.Sprintf("%s/v3/service_instances?names=%s&space_guids=%s&type=managed&per_page=1",
		c.BaseURL(), name, spaceGUID)
	var page serviceInstancesResponse
	if err := c.get(ctx, url, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	return &page.Resources[0], nil
}

// CreateServiceInstance creates a managed service instance asynchronously.
// CF returns 202 Accepted for async creation; this is treated as success.
func (c *Client) CreateServiceInstance(ctx context.Context, name, spaceGUID, planGUID string) error {
	body := map[string]interface{}{
		"type": "managed",
		"name": name,
		"relationships": map[string]interface{}{
			"space":        map[string]interface{}{"data": map[string]string{"guid": spaceGUID}},
			"service_plan": map[string]interface{}{"data": map[string]string{"guid": planGUID}},
		},
	}
	err := c.post(ctx, c.BaseURL()+"/v3/service_instances", body, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusAccepted {
			return nil
		}
		return err
	}
	return nil
}

// ── service credential bindings (keys) ───────────────────────────────────────

// FindServiceCredentialBinding looks up a service key by name on a service instance.
// Returns nil, nil if not found.
func (c *Client) FindServiceCredentialBinding(ctx context.Context, name, instanceGUID string) (*ServiceCredentialBinding, error) {
	url := fmt.Sprintf("%s/v3/service_credential_bindings?names=%s&service_instance_guids=%s&type=key&per_page=1",
		c.BaseURL(), name, instanceGUID)
	var page serviceCredentialBindingsResponse
	if err := c.get(ctx, url, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	return &page.Resources[0], nil
}

// CreateServiceCredentialBinding creates a service key asynchronously.
// CF returns 202 Accepted for async creation; this is treated as success.
func (c *Client) CreateServiceCredentialBinding(ctx context.Context, name, instanceGUID string) error {
	body := map[string]interface{}{
		"type": "key",
		"name": name,
		"relationships": map[string]interface{}{
			"service_instance": map[string]interface{}{"data": map[string]string{"guid": instanceGUID}},
		},
	}
	err := c.post(ctx, c.BaseURL()+"/v3/service_credential_bindings", body, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusAccepted {
			return nil
		}
		return err
	}
	return nil
}

// GetServiceCredentialDetails fetches the raw credential map for a service key.
func (c *Client) GetServiceCredentialDetails(ctx context.Context, bindingGUID string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v3/service_credential_bindings/%s/details", c.BaseURL(), bindingGUID)
	var details serviceCredentialDetails
	if err := c.get(ctx, url, &details); err != nil {
		return nil, err
	}
	return details.Credentials, nil
}

// ListServiceInstancesByPlanGUID lists all managed service instances with the
// given plan GUID. If orgGUID is non-empty, results are restricted to that org.
func (c *Client) ListServiceInstancesByPlanGUID(ctx context.Context, planGUID, orgGUID string) ([]ServiceInstance, error) {
	base := fmt.Sprintf("%s/v3/service_instances?service_plan_guids=%s&type=managed&per_page=5000",
		c.BaseURL(), planGUID)
	if orgGUID != "" {
		base += "&organization_guids=" + orgGUID
	}

	var all []ServiceInstance
	nextURL := base
	for nextURL != "" {
		var page serviceInstancesResponse
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

// FindAnyServiceCredentialBinding returns the first service key found for a
// service instance, or nil if no keys exist.
func (c *Client) FindAnyServiceCredentialBinding(ctx context.Context, instanceGUID string) (*ServiceCredentialBinding, error) {
	url := fmt.Sprintf("%s/v3/service_credential_bindings?service_instance_guids=%s&type=key&per_page=1",
		c.BaseURL(), instanceGUID)
	var page serviceCredentialBindingsResponse
	if err := c.get(ctx, url, &page); err != nil {
		return nil, err
	}
	if len(page.Resources) == 0 {
		return nil, nil
	}
	return &page.Resources[0], nil
}
