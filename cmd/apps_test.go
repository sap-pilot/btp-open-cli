package cmd

import (
	"strings"
	"testing"
)

// appsPageJSON returns a CF v3 /v3/apps response with one app.
func appsPageJSON(appGUID, appName, spaceGUID string) string {
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]interface{}{
			{
				"guid":       appGUID,
				"name":       appName,
				"state":      "STARTED",
				"created_at": "2024-01-01T00:00:00Z",
				"updated_at": "2024-01-01T00:00:00Z",
				"metadata":   map[string]interface{}{"annotations": map[string]string{"mta_id": ""}},
				"relationships": map[string]interface{}{
					"space": map[string]interface{}{
						"data": map[string]string{"guid": spaceGUID},
					},
				},
			},
		},
	})
}

// processesPageJSON returns a CF v3 /v3/processes response.
func processesPageJSON(appGUID string) string {
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]interface{}{
			{
				"guid":          "proc1",
				"instances":     2,
				"memory_in_mb":  512,
				"disk_in_mb":    1024,
				"relationships": map[string]interface{}{"app": map[string]interface{}{"data": map[string]string{"guid": appGUID}}},
			},
		},
	})
}

func TestApps_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "apps")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestApps_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "apps")
	if err != nil {
		t.Fatalf("apps command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected app name in output, got: %q", stdout)
	}
}

func TestApps_Filter(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]interface{}{
				{
					"guid": "app1", "name": "my-app", "state": "STARTED",
					"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z",
					"metadata":      map[string]interface{}{"annotations": map[string]string{}},
					"relationships": map[string]interface{}{"space": map[string]interface{}{"data": map[string]string{"guid": "sp1"}}},
				},
				{
					"guid": "app2", "name": "other-app", "state": "STOPPED",
					"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z",
					"metadata":      map[string]interface{}{"annotations": map[string]string{}},
					"relationships": map[string]interface{}{"space": map[string]interface{}{"data": map[string]string{"guid": "sp1"}}},
				},
			},
		}),
		"/v3/processes": processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "apps", "--filter", "my-app")
	if err != nil {
		t.Fatalf("apps --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected my-app in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "other-app") {
		t.Errorf("other-app should be filtered out, got: %q", stdout)
	}
}

func TestApps_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "apps", "--format", "csv")
	if err != nil {
		t.Fatalf("apps --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "app_name") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected app name in CSV, got: %q", stdout)
	}
}

func TestApps_OrgFilter(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
		"/v3/spaces":    spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":      appsPageJSON("app1", "app-in-org1", "sp1"),
		"/v3/processes": processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "apps", "--org", "org1")
	if err != nil {
		t.Fatalf("apps --org failed: %v", err)
	}
	if !strings.Contains(stdout, "app-in-org1") {
		t.Errorf("expected app-in-org1 in output, got: %q", stdout)
	}
}
