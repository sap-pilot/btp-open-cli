package cmd

import (
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

// setTwoOrgDefaultScope saves a two-org default scope (as `bo orgs` would)
// against apiURL, for tests exercising the multi-target describe-subaccount
// path without needing the interactive picker.
func setTwoOrgDefaultScope(t *testing.T, apiURL string) {
	t.Helper()
	creds, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	region := store.APIURLToRegion(apiURL)
	creds.DefaultOrgScope = []store.OrgScopeRef{
		{Region: region, ID: "org1", Name: "org-one", APIURL: apiURL},
		{Region: region, ID: "org2", Name: "org-two", APIURL: apiURL},
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
}

func TestDescribeSubaccount_MissingOrgFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "describe-subaccount")
	if err == nil {
		t.Fatal("expected error when --org is not provided")
	}
}

func TestDescribeSubaccount_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "describe-subaccount", "--org", "my-org")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestDescribeSubaccount_DefaultScope_ResolvesTargetsWithoutOrg(t *testing.T) {
	// No CIS routes are registered, so this fails past org resolution once it
	// tries to discover a CIS central-viewer key — proving the default org
	// scope successfully resolved target orgs without requiring --org.
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
	})
	setupTestEnv(t, srv.URL)
	setTwoOrgDefaultScope(t, srv.URL)

	_, _, err := runCmd(t, "describe-subaccount", "--no-prompt")
	if err == nil {
		t.Fatal("expected error once CIS central-viewer key discovery fails")
	}
	if !strings.Contains(err.Error(), "CIS central-viewer") {
		t.Errorf("expected CIS central-viewer error (proving org resolution succeeded), got: %v", err)
	}
}

func TestDescribeSubaccount_SubaccountFlagRequiresSingleTarget(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
	})
	setupTestEnv(t, srv.URL)
	setTwoOrgDefaultScope(t, srv.URL)

	_, _, err := runCmd(t, "describe-subaccount", "--no-prompt", "--subaccount", "sub1")
	if err == nil {
		t.Fatal("expected error when --subaccount is combined with multiple target orgs")
	}
	if !strings.Contains(err.Error(), "--subaccount") {
		t.Errorf("expected error to mention --subaccount, got: %v", err)
	}
}
