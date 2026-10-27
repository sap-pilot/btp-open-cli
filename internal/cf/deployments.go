package cf

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// buildPollInterval is the delay between polls of a CF v3 build while
// RestageAppWithStrategy waits for staging to finish. A package-level var
// so tests can speed it up.
var buildPollInterval = 2 * time.Second

// buildPollTimeout bounds how long RestageAppWithStrategy waits for a build
// to finish staging before giving up.
var buildPollTimeout = 10 * time.Minute

type dropletResource struct {
	GUID  string `json:"guid"`
	State string `json:"state"`
	Links struct {
		Package struct {
			Href string `json:"href"`
		} `json:"package"`
	} `json:"links"`
}

type buildResource struct {
	GUID    string `json:"guid"`
	State   string `json:"state"`
	Droplet struct {
		GUID string `json:"guid"`
	} `json:"droplet"`
}

// CreateDeployment creates a v3 Deployment to redeploy appGUID using the
// given strategy ("rolling" or "canary"), achieving a zero-downtime
// restart. If dropletGUID is non-empty, that specific droplet is deployed
// (used by RestageApp/RestageAppWithStrategy to roll out a newly-built
// droplet); otherwise the app's current droplet is redeployed in place.
// See https://v3-apidocs.cloudfoundry.org/#deployments.
func (c *Client) CreateDeployment(ctx context.Context, appGUID, strategy, dropletGUID string) (string, error) {
	body := map[string]interface{}{
		"strategy": strategy,
		"relationships": map[string]interface{}{
			"app": map[string]interface{}{
				"data": map[string]string{"guid": appGUID},
			},
		},
	}
	if dropletGUID != "" {
		body["droplet"] = map[string]interface{}{"guid": dropletGUID}
	}

	var resp struct {
		GUID string `json:"guid"`
	}
	if err := c.post(ctx, c.BaseURL()+"/v3/deployments", body, &resp); err != nil {
		return "", err
	}
	return resp.GUID, nil
}

// SetCurrentDroplet assigns dropletGUID as appGUID's current droplet.
func (c *Client) SetCurrentDroplet(ctx context.Context, appGUID, dropletGUID string) error {
	url := fmt.Sprintf("%s/v3/apps/%s/relationships/current_droplet", c.BaseURL(), appGUID)
	body := map[string]interface{}{"data": map[string]interface{}{"guid": dropletGUID}}
	return c.patch(ctx, url, body, nil)
}

// buildDropletFromCurrentPackage restages appGUID's current package into a
// fresh droplet: it fetches the current droplet to find the package it was
// built from, creates a new build from that package, and polls the build
// until staging finishes, returning the new droplet's GUID.
//
// CF's v3 API has no single-call "restage" action — the dedicated
// /v3/apps/:guid/actions/restage endpoint some older CF CLI versions and
// docs mention has been removed on current platforms (it 404s as "Unknown
// request") — so both RestageApp and RestageAppWithStrategy build the new
// droplet this way; see
// https://github.com/cloudfoundry/cloud_controller_ng/wiki/How-to-restage-an-app-directly-using-V3-APIs-(with-examples).
func (c *Client) buildDropletFromCurrentPackage(ctx context.Context, appGUID string) (string, error) {
	var current dropletResource
	if err := c.get(ctx, fmt.Sprintf("%s/v3/apps/%s/droplets/current", c.BaseURL(), appGUID), &current); err != nil {
		return "", fmt.Errorf("getting current droplet: %w", err)
	}
	packageGUID, err := packageGUIDFromHref(current.Links.Package.Href)
	if err != nil {
		return "", err
	}

	buildBody := map[string]interface{}{"package": map[string]interface{}{"guid": packageGUID}}
	var b buildResource
	if err := c.post(ctx, c.BaseURL()+"/v3/builds", buildBody, &b); err != nil {
		return "", fmt.Errorf("creating build: %w", err)
	}

	deadline := time.Now().Add(buildPollTimeout)
	for b.State == "STAGING" {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for build %s to stage", b.GUID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(buildPollInterval):
		}
		if err := c.get(ctx, fmt.Sprintf("%s/v3/builds/%s", c.BaseURL(), b.GUID), &b); err != nil {
			return "", fmt.Errorf("polling build %s: %w", b.GUID, err)
		}
	}
	if b.State != "STAGED" {
		return "", fmt.Errorf("build %s did not stage successfully (state %s)", b.GUID, b.State)
	}
	return b.Droplet.GUID, nil
}

// RestageApp restages appGUID: it builds a new droplet from the app's
// current package, waits for staging to finish, assigns the new droplet as
// current, and restarts the app to run it — causing downtime, the same as
// the classic `cf restage` with no strategy.
func (c *Client) RestageApp(ctx context.Context, appGUID string) (*App, error) {
	dropletGUID, err := c.buildDropletFromCurrentPackage(ctx, appGUID)
	if err != nil {
		return nil, err
	}
	if err := c.SetCurrentDroplet(ctx, appGUID, dropletGUID); err != nil {
		return nil, fmt.Errorf("assigning new droplet: %w", err)
	}
	return c.RestartApp(ctx, appGUID)
}

// RestageAppWithStrategy restages appGUID with zero downtime: it builds a
// new droplet from the app's current package, waits for staging to finish,
// then deploys that droplet using the given strategy ("rolling" or
// "canary") instead of assigning it directly and restarting.
func (c *Client) RestageAppWithStrategy(ctx context.Context, appGUID, strategy string) error {
	dropletGUID, err := c.buildDropletFromCurrentPackage(ctx, appGUID)
	if err != nil {
		return err
	}
	if _, err := c.CreateDeployment(ctx, appGUID, strategy, dropletGUID); err != nil {
		return fmt.Errorf("deploying new droplet: %w", err)
	}
	return nil
}

// packageGUIDFromHref extracts the trailing GUID segment from a CF v3
// resource link such as ".../v3/packages/<guid>".
func packageGUIDFromHref(href string) (string, error) {
	idx := strings.LastIndex(href, "/")
	if idx < 0 || idx == len(href)-1 {
		return "", fmt.Errorf("unexpected package link %q", href)
	}
	return href[idx+1:], nil
}
