package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// appEnvJSON returns a CF v3 /v3/apps/:guid/env response with the given
// VCAP_SERVICES groups (keyed by service offering, e.g. "hana").
func appEnvJSON(vcapServices map[string]interface{}) string {
	return mustJSONStr(map[string]interface{}{
		"system_env_json": map[string]interface{}{
			"VCAP_SERVICES": vcapServices,
		},
	})
}

func hanaBinding(name string) map[string]interface{} {
	return map[string]interface{}{
		"label":         "hana",
		"name":          name,
		"tags":          []string{"hana", "database", "relational"},
		"instance_guid": "847d683e-e6d5-4d61-8e69-23242ea65404",
		"credentials":   map[string]interface{}{"host": "example.hanacloud.ondemand.com"},
	}
}

func xsuaaBinding(name string) map[string]interface{} {
	return map[string]interface{}{
		"label":       "xsuaa",
		"name":        name,
		"tags":        []string{"xsuaa"},
		"credentials": map[string]interface{}{"clientid": "sb-test-client"},
	}
}

func TestServiceCredentials_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "service-credentials")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestServiceCredentials_NoScopeNoFlags(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "service-credentials")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestServiceCredentials_Basic(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/env": appEnvJSON(map[string]interface{}{
			"hana":  []map[string]interface{}{hanaBinding("my-hdi")},
			"xsuaa": []map[string]interface{}{xsuaaBinding("my-uaa")},
		}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "service-credentials")
	if err != nil {
		t.Fatalf("service-credentials failed: %v\nstderr: %s", err, stderr)
	}

	var records []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 service records, got %d: %+v", len(records), records)
	}
	for _, rec := range records {
		if rec["org"] != "my-org" {
			t.Errorf("expected org 'my-org', got: %v", rec["org"])
		}
		if rec["space"] != "dev" {
			t.Errorf("expected space 'dev', got: %v", rec["space"])
		}
	}
}

func TestServiceCredentials_ServicesFilter(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/env": appEnvJSON(map[string]interface{}{
			"hana":  []map[string]interface{}{hanaBinding("my-hdi")},
			"xsuaa": []map[string]interface{}{xsuaaBinding("my-uaa")},
		}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-credentials", "--services", "hana")
	if err != nil {
		t.Fatalf("service-credentials --services failed: %v", err)
	}

	var records []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 service record, got %d: %+v", len(records), records)
	}
	if records[0]["label"] != "hana" {
		t.Errorf("expected label 'hana', got: %v", records[0]["label"])
	}
}

func TestServiceCredentials_SpacesFilterExcludesNonMatching(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/env": appEnvJSON(map[string]interface{}{
			"hana": []map[string]interface{}{hanaBinding("my-hdi")},
		}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-credentials", "--spaces", "qa")
	if err != nil {
		t.Fatalf("service-credentials --spaces failed: %v", err)
	}

	var records []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records when --spaces matches nothing, got %d: %+v", len(records), records)
	}
}

func TestServiceCredentials_AppsPatternGlob(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]interface{}{
				{
					"guid": "app1", "name": "my-app-srv", "state": "STARTED",
					"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z",
					"metadata":      map[string]interface{}{"annotations": map[string]string{}},
					"relationships": map[string]interface{}{"space": map[string]interface{}{"data": map[string]string{"guid": "sp1"}}},
				},
				{
					"guid": "app2", "name": "my-app-ui", "state": "STARTED",
					"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z",
					"metadata":      map[string]interface{}{"annotations": map[string]string{}},
					"relationships": map[string]interface{}{"space": map[string]interface{}{"data": map[string]string{"guid": "sp1"}}},
				},
			},
		}),
		"/v3/apps/app1/env": appEnvJSON(map[string]interface{}{
			"hana": []map[string]interface{}{hanaBinding("srv-hdi")},
		}),
		"/v3/apps/app2/env": appEnvJSON(map[string]interface{}{
			"hana": []map[string]interface{}{hanaBinding("ui-hdi")},
		}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-credentials", "--apps", "*-srv")
	if err != nil {
		t.Fatalf("service-credentials --apps failed: %v", err)
	}

	var records []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record (only my-app-srv matches), got %d: %+v", len(records), records)
	}
	if records[0]["name"] != "srv-hdi" {
		t.Errorf("expected the srv app's hdi instance, got: %v", records[0]["name"])
	}
}

func TestScMatchesSpaceName(t *testing.T) {
	if !scMatchesSpaceName("", "dev") {
		t.Error("empty pattern should match everything")
	}
	if !scMatchesSpaceName("qa,dev", "DEV") {
		t.Error("expected case-insensitive exact match")
	}
	if scMatchesSpaceName("qa,prod", "dev") {
		t.Error("expected no match")
	}
	if scMatchesSpaceName("de", "dev") {
		t.Error("expected exact match, not substring")
	}
}

func TestScMatchesAppPattern(t *testing.T) {
	if !scMatchesAppPattern("", "any-app") {
		t.Error("empty pattern should match everything")
	}
	if !scMatchesAppPattern("*-srv,*-app", "my-app") {
		t.Error("expected glob match against *-app")
	}
	if scMatchesAppPattern("*-srv", "my-app-ui") {
		t.Error("expected no glob match")
	}
	if !scMatchesAppPattern("my-app", "MY-APP-UI") {
		t.Error("expected case-insensitive substring match")
	}
}
