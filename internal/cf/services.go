package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// serviceCredentialBindingPollInterval is the delay between polls of a CF v3
// job while waiting for an asynchronous service credential binding create or
// delete to finish. Tests override this to keep polling fast.
var serviceCredentialBindingPollInterval = 2 * time.Second

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

// jobResource is a CF v3 job, as returned from the Location header of an
// asynchronous (202 Accepted) create/delete response.
type jobResource struct {
	State  string `json:"state"` // PROCESSING, POLLING, COMPLETE, or FAILED
	Errors []struct {
		Detail string `json:"detail"`
	} `json:"errors"`
}

// pollJob polls a CF v3 job at jobURL until it reaches a terminal state
// (COMPLETE or FAILED), respecting ctx cancellation between polls.
func (c *Client) pollJob(ctx context.Context, jobURL string) error {
	for {
		var job jobResource
		if err := c.get(ctx, jobURL, &job); err != nil {
			return fmt.Errorf("checking job status: %w", err)
		}
		switch job.State {
		case "COMPLETE":
			return nil
		case "FAILED":
			if len(job.Errors) > 0 {
				return fmt.Errorf("job failed: %s", job.Errors[0].Detail)
			}
			return fmt.Errorf("job failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serviceCredentialBindingPollInterval):
		}
	}
}

// CreateServiceCredentialBinding creates a service key. CF may bind
// synchronously (201 Created) or asynchronously (202 Accepted with a job to
// poll) depending on the broker; either way, this waits for completion and
// returns the created binding's GUID.
func (c *Client) CreateServiceCredentialBinding(ctx context.Context, name, instanceGUID string) (string, error) {
	body := map[string]interface{}{
		"type": "key",
		"name": name,
		"relationships": map[string]interface{}{
			"service_instance": map[string]interface{}{"data": map[string]string{"guid": instanceGUID}},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL()+"/v3/service_credential_bindings", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, respBody, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return "", fmt.Errorf("POST /v3/service_credential_bindings: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusCreated:
		var created struct {
			GUID string `json:"guid"`
		}
		if err := json.Unmarshal(respBody, &created); err != nil {
			return "", err
		}
		return created.GUID, nil
	case http.StatusAccepted:
		jobURL := resp.Header.Get("Location")
		if jobURL == "" {
			return "", fmt.Errorf("async service key creation returned no job Location header")
		}
		if err := c.pollJob(ctx, jobURL); err != nil {
			return "", err
		}
		binding, err := c.FindServiceCredentialBinding(ctx, name, instanceGUID)
		if err != nil {
			return "", err
		}
		if binding == nil {
			return "", fmt.Errorf("service key %q not found after its creation job completed", name)
		}
		return binding.GUID, nil
	default:
		return "", &APIError{StatusCode: resp.StatusCode, Body: respBody}
	}
}

// DeleteServiceCredentialBinding deletes a service key, waiting for CF's job
// to finish if deletion is asynchronous — so a caller that immediately
// recreates a binding with the same name doesn't race the deletion.
func (c *Client) DeleteServiceCredentialBinding(ctx context.Context, bindingGUID string) error {
	url := fmt.Sprintf("%s/v3/service_credential_bindings/%s", c.BaseURL(), bindingGUID)

	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		return req, nil
	}

	resp, body, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", url, err)
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusAccepted:
		jobURL := resp.Header.Get("Location")
		if jobURL == "" {
			return nil
		}
		return c.pollJob(ctx, jobURL)
	default:
		return &APIError{StatusCode: resp.StatusCode, Body: body}
	}
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

// ── service instances with relationships ─────────────────────────────────────

// ServiceInstanceFull includes space and service-plan GUIDs from CF relationships.
type ServiceInstanceFull struct {
	GUID          string   `json:"guid"`
	Name          string   `json:"name"`
	Type          string   `json:"type"` // "managed" or "user-provided"
	Tags          []string `json:"tags"`
	LastOperation struct {
		State string `json:"state"`
	} `json:"last_operation"`
	Relationships struct {
		Space struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		} `json:"space"`
		ServicePlan struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		} `json:"service_plan"`
	} `json:"relationships"`
}

// ServicePlanDetail pairs a plan name with its service offering name.
type ServicePlanDetail struct {
	Name        string
	ServiceName string
}

type serviceInstancesFullResponse struct {
	Pagination pagination            `json:"pagination"`
	Resources  []ServiceInstanceFull `json:"resources"`
}

type servicePlanWithOfferingEntry struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	Relationships struct {
		ServiceOffering struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		} `json:"service_offering"`
	} `json:"relationships"`
}

type serviceOfferingEntry struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type servicePlansWithOfferingResponse struct {
	Pagination pagination                     `json:"pagination"`
	Resources  []servicePlanWithOfferingEntry `json:"resources"`
	Included   struct {
		ServiceOfferings []serviceOfferingEntry `json:"service_offerings"`
	} `json:"included"`
}

// ListServiceInstancesByOrg fetches all managed service instances in an org,
// including space and service-plan GUIDs from relationships.
func (c *Client) ListServiceInstancesByOrg(ctx context.Context, orgGUID string) ([]ServiceInstanceFull, error) {
	var all []ServiceInstanceFull
	nextURL := fmt.Sprintf("%s/v3/service_instances?organization_guids=%s&type=managed&per_page=5000",
		c.BaseURL(), orgGUID)
	for nextURL != "" {
		var page serviceInstancesFullResponse
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

// ListServiceInstancesFullBySpaces fetches all managed service instances
// whose space is in spaceGUIDs, including space/service-plan relationships
// and tags.
func (c *Client) ListServiceInstancesFullBySpaces(ctx context.Context, spaceGUIDs []string) ([]ServiceInstanceFull, error) {
	var all []ServiceInstanceFull
	nextURL := fmt.Sprintf("%s/v3/service_instances?space_guids=%s&type=managed&per_page=5000",
		c.BaseURL(), strings.Join(spaceGUIDs, ","))
	for nextURL != "" {
		var page serviceInstancesFullResponse
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

// ListServiceInstancesFullBySpacesAnyType fetches all service instances
// (managed and user-provided alike) whose space is in spaceGUIDs, including
// space/service-plan relationships, type, and tags. Unlike
// ListServiceInstancesFullBySpaces, this does not filter by type — a
// user-provided instance's Relationships.ServicePlan.Data.GUID is simply
// empty.
func (c *Client) ListServiceInstancesFullBySpacesAnyType(ctx context.Context, spaceGUIDs []string) ([]ServiceInstanceFull, error) {
	var all []ServiceInstanceFull
	nextURL := fmt.Sprintf("%s/v3/service_instances?space_guids=%s&per_page=5000",
		c.BaseURL(), strings.Join(spaceGUIDs, ","))
	for nextURL != "" {
		var page serviceInstancesFullResponse
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

// ListServicePlanDetails fetches plan names and offering names for the given
// plan GUIDs. Returns a map of plan GUID → ServicePlanDetail.
func (c *Client) ListServicePlanDetails(ctx context.Context, planGUIDs []string) (map[string]ServicePlanDetail, error) {
	result := make(map[string]ServicePlanDetail, len(planGUIDs))
	if len(planGUIDs) == 0 {
		return result, nil
	}
	const batchSize = 50
	for i := 0; i < len(planGUIDs); i += batchSize {
		end := i + batchSize
		if end > len(planGUIDs) {
			end = len(planGUIDs)
		}
		u := fmt.Sprintf("%s/v3/service_plans?guids=%s&include=service_offering&per_page=5000",
			c.BaseURL(), strings.Join(planGUIDs[i:end], ","))
		var page servicePlansWithOfferingResponse
		if err := c.get(ctx, u, &page); err != nil {
			return nil, err
		}
		offeringNames := make(map[string]string, len(page.Included.ServiceOfferings))
		for _, o := range page.Included.ServiceOfferings {
			offeringNames[o.GUID] = o.Name
		}
		for _, plan := range page.Resources {
			result[plan.GUID] = ServicePlanDetail{
				Name:        plan.Name,
				ServiceName: offeringNames[plan.Relationships.ServiceOffering.Data.GUID],
			}
		}
	}
	return result, nil
}

// ListServiceInstancesInSpace lists all managed service instances with the given
// plan GUID in the specified space. Pass an empty planGUID to list all managed
// instances regardless of plan.
func (c *Client) ListServiceInstancesInSpace(ctx context.Context, spaceGUID, planGUID string) ([]ServiceInstance, error) {
	base := fmt.Sprintf("%s/v3/service_instances?space_guids=%s&type=managed&per_page=5000",
		c.BaseURL(), spaceGUID)
	if planGUID != "" {
		base += "&service_plan_guids=" + planGUID
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

// ── user-provided service instances ──────────────────────────────────────────

// ListUserProvidedServiceInstancesInSpace lists all user-provided service
// instances in the given space, for detecting name collisions before create.
func (c *Client) ListUserProvidedServiceInstancesInSpace(ctx context.Context, spaceGUID string) ([]ServiceInstance, error) {
	var all []ServiceInstance
	nextURL := fmt.Sprintf("%s/v3/service_instances?space_guids=%s&type=user-provided&per_page=5000",
		c.BaseURL(), spaceGUID)
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

// CreateUserProvidedServiceInstance creates a user-provided service instance.
// Unlike managed instances, this is synchronous — CF returns 201 Created.
func (c *Client) CreateUserProvidedServiceInstance(ctx context.Context, name, spaceGUID string, credentials map[string]interface{}, tags []string) error {
	body := map[string]interface{}{
		"type":        "user-provided",
		"name":        name,
		"credentials": credentials,
		"tags":        tags,
		"relationships": map[string]interface{}{
			"space": map[string]interface{}{"data": map[string]string{"guid": spaceGUID}},
		},
	}
	return c.post(ctx, c.BaseURL()+"/v3/service_instances", body, nil)
}

// UpdateUserProvidedServiceInstance replaces the credentials and tags of an
// existing user-provided service instance. This is synchronous — CF returns
// 200 OK.
func (c *Client) UpdateUserProvidedServiceInstance(ctx context.Context, instanceGUID string, credentials map[string]interface{}, tags []string) error {
	body := map[string]interface{}{
		"credentials": credentials,
		"tags":        tags,
	}
	url := fmt.Sprintf("%s/v3/service_instances/%s", c.BaseURL(), instanceGUID)
	return c.patch(ctx, url, body, nil)
}

// ServiceCredentialBindingFull is a service credential binding (key or app
// binding) annotated with its type and the service instance it belongs to,
// for batch lookups across many instances at once.
type ServiceCredentialBindingFull struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	Type          string `json:"type"` // "key" or "app"
	Relationships struct {
		ServiceInstance struct {
			Data struct {
				GUID string `json:"guid"`
			} `json:"data"`
		} `json:"service_instance"`
	} `json:"relationships"`
}

type serviceCredentialBindingsFullResponse struct {
	Pagination pagination                     `json:"pagination"`
	Resources  []ServiceCredentialBindingFull `json:"resources"`
}

// ListServiceCredentialBindingsByInstances lists every service credential
// binding (both service keys and app bindings) on any of the given service
// instances, batched to stay within a reasonable URL length.
func (c *Client) ListServiceCredentialBindingsByInstances(ctx context.Context, instanceGUIDs []string) ([]ServiceCredentialBindingFull, error) {
	var all []ServiceCredentialBindingFull
	if len(instanceGUIDs) == 0 {
		return all, nil
	}
	const batchSize = 50
	for i := 0; i < len(instanceGUIDs); i += batchSize {
		end := i + batchSize
		if end > len(instanceGUIDs) {
			end = len(instanceGUIDs)
		}
		nextURL := fmt.Sprintf("%s/v3/service_credential_bindings?service_instance_guids=%s&per_page=5000",
			c.BaseURL(), strings.Join(instanceGUIDs[i:end], ","))
		for nextURL != "" {
			var page serviceCredentialBindingsFullResponse
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
	}
	return all, nil
}

// DeleteServiceInstance deletes a service instance. CF deletes a
// user-provided instance synchronously (204/200) but a managed instance
// asynchronously (202 Accepted with a job to poll); either way this waits
// for completion. The instance must have no remaining service credential
// bindings/keys or routes, or CF rejects the delete — callers should delete
// those first (see ListServiceCredentialBindingsByInstances and
// DeleteServiceCredentialBinding).
func (c *Client) DeleteServiceInstance(ctx context.Context, instanceGUID string) error {
	url := fmt.Sprintf("%s/v3/service_instances/%s", c.BaseURL(), instanceGUID)

	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		return req, nil
	}

	resp, body, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", url, err)
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusAccepted:
		jobURL := resp.Header.Get("Location")
		if jobURL == "" {
			return nil
		}
		return c.pollJob(ctx, jobURL)
	default:
		return &APIError{StatusCode: resp.StatusCode, Body: body}
	}
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
