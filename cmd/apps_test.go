package cmd

import (
	"os"
	"path/filepath"
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

func TestApps_NoScopeNoFlags(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "apps")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
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
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "apps")
	if err != nil {
		t.Fatalf("apps command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected app name in output, got: %q", stdout)
	}
}

func TestApps_Include(t *testing.T) {
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
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "apps", "--include", "my-app")
	if err != nil {
		t.Fatalf("apps --include failed: %v", err)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected my-app in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "other-app") {
		t.Errorf("other-app should be filtered out, got: %q", stdout)
	}
}

func TestApps_Exclude(t *testing.T) {
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
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "apps", "--exclude", "STOPPED,other")
	if err != nil {
		t.Fatalf("apps --exclude failed: %v", err)
	}
	if !strings.Contains(stdout, "my-app") {
		t.Errorf("expected my-app in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "other-app") {
		t.Errorf("other-app should be excluded, got: %q", stdout)
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
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

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

func TestApps_OutputExtensionAutoDetectsFormat(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	for _, tc := range []struct {
		ext           string
		expectInBody  string
		explicitFlags []string
	}{
		{ext: "csv", expectInBody: "app_name"},
		{ext: "json", expectInBody: `"app_name"`},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "apps."+tc.ext)

		args := []string{"apps", "--output", path}
		args = append(args, tc.explicitFlags...)
		_, stderr, err := runCmd(t, args...)
		if err != nil {
			t.Fatalf("[.%s] apps --output failed: %v\nstderr: %s", tc.ext, err, stderr)
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("[.%s] reading output file: %v", tc.ext, readErr)
		}
		if !strings.Contains(string(body), tc.expectInBody) {
			t.Errorf("[.%s] expected %q in output, got: %q", tc.ext, tc.expectInBody, string(body))
		}
	}
}

func TestApps_ExplicitFormatOverridesOutputExtension(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	dir := t.TempDir()
	path := filepath.Join(dir, "apps.json") // extension says json...

	_, stderr, err := runCmd(t, "apps", "--output", path, "--format", "csv") // ...but --format wins
	if err != nil {
		t.Fatalf("apps failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if !strings.Contains(string(body), "app_name") || strings.Contains(string(body), `"app_name"`) {
		t.Errorf("expected plain CSV (not JSON) despite .json extension, got: %q", string(body))
	}
}

func TestApps_IncludeEnv(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
		"/v3/apps/app1/environment_variables": mustJSONStr(map[string]interface{}{"var": map[string]string{
			"FOO":               "bar",
			"MY_SECRET":         "s3cr3t",
			"DEPLOY_ATTRIBUTES": "{\"foo\":\"bar\"}",
		}}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "apps", "--include-env", "--format", "json")
	if err != nil {
		t.Fatalf("apps --include-env failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, `"app_env"`) || !strings.Contains(stdout, `"FOO": "bar"`) {
		t.Errorf("expected app_env with FOO=bar in output, got: %q", stdout)
	}
	// Secret-named variables must be included in full, never masked.
	if !strings.Contains(stdout, `"MY_SECRET": "s3cr3t"`) {
		t.Errorf("expected MY_SECRET's real value in output (never masked), got: %q", stdout)
	}
	// DEPLOY_ATTRIBUTES is deploy-tooling noise and must be excluded.
	if strings.Contains(stdout, "DEPLOY_ATTRIBUTES") {
		t.Errorf("expected DEPLOY_ATTRIBUTES to be skipped, got: %q", stdout)
	}
}

func TestApps_WithoutIncludeEnv_NoEnvField(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "apps", "--format", "json")
	if err != nil {
		t.Fatalf("apps failed: %v", err)
	}
	if strings.Contains(stdout, "app_env") {
		t.Errorf("expected no app_env field without --include-env, got: %q", stdout)
	}
}

// singleAppJSON returns a CF v3 GET /v3/apps/:guid response for one app
// with the given metadata.labels — used to mock --include-labels'
// dedicated per-app fetch (cf.Client.GetApp), which deliberately does not
// reuse whatever metadata.labels the bulk /v3/apps listing itself carries.
func singleAppJSON(appGUID, appName string, labels map[string]string) string {
	return mustJSONStr(map[string]interface{}{
		"guid":       appGUID,
		"name":       appName,
		"state":      "STARTED",
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-01T00:00:00Z",
		"metadata": map[string]interface{}{
			"annotations": map[string]string{"mta_id": ""},
			"labels":      labels,
		},
	})
}

func TestApps_IncludeLabels(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1":     singleAppJSON("app1", "my-app", map[string]string{"team": "payments", "tier": "1"}),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "apps", "--include-labels", "--format", "json")
	if err != nil {
		t.Fatalf("apps --include-labels failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, `"app_labels"`) || !strings.Contains(stdout, `"team": "payments"`) {
		t.Errorf("expected app_labels with team=payments in output, got: %q", stdout)
	}
}

func TestApps_WithoutIncludeLabels_NoLabelsField(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "apps", "--format", "json")
	if err != nil {
		t.Fatalf("apps failed: %v", err)
	}
	if strings.Contains(stdout, "app_labels") {
		t.Errorf("expected no app_labels field without --include-labels, got: %q", stdout)
	}
}

func TestApps_IncludeLabels_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1":     singleAppJSON("app1", "my-app", map[string]string{"team": "payments"}),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "apps", "--include-labels", "--format", "csv")
	if err != nil {
		t.Fatalf("apps --include-labels --format csv failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "app_labels") {
		t.Errorf("expected app_labels column in CSV header, got: %q", stdout)
	}
	if !strings.Contains(stdout, `"{""team"":""payments""}"`) {
		t.Errorf("expected JSON-encoded labels in CSV cell, got: %q", stdout)
	}
}

// TestApps_IncludeLabels_RefetchesPerApp confirms --include-labels fetches
// each app's labels directly (GET /v3/apps/:guid) rather than trusting
// whatever metadata.labels happens to be embedded in the bulk /v3/apps
// listing — reproducing the reported bug where a label set moments earlier
// (e.g. via 'cf set-label') wasn't reflected in that listing yet. The list
// response here deliberately carries a stale/different labels map from the
// per-app one, and only the per-app map must win.
func TestApps_IncludeLabels_RefetchesPerApp(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":          appsPageJSON("app1", "my-app", "sp1"), // no labels at all in the list response
		"/v3/apps/app1":     singleAppJSON("app1", "my-app", map[string]string{"commit_id": "cd912038"}),
		"/v3/processes":     processesPageJSON("app1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "apps", "--include-labels", "--format", "json")
	if err != nil {
		t.Fatalf("apps --include-labels failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, `"commit_id": "cd912038"`) {
		t.Errorf("expected commit_id label fetched from the per-app endpoint, got: %q", stdout)
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
