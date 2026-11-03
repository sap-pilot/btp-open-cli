package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

// writeAppsEnvFile marshals doc to JSON and writes it to a temp file, returning its path.
func writeAppsEnvFile(t *testing.T, doc appsOutDoc) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshaling apps-env fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "apps-env.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing apps-env fixture: %v", err)
	}
	return path
}

// basicAppsEnvDoc returns a one-app apps-env document for region/org/space
// names that line up with the fakeCFServer-backed test helpers.
func basicAppsEnvDoc(region, orgID, orgName string) appsOutDoc {
	return appsOutDoc{
		Regions: []appsOutRegion{
			{
				ID: region,
				Orgs: []appsOutOrg{
					{
						ID:   orgID,
						Name: orgName,
						Spaces: []appsOutSpace{
							{
								ID:   "sp1",
								Name: "dev",
								Apps: []appsOutApp{
									{
										ID:   "stale-app-guid", // deliberately wrong — must be re-resolved by name
										Name: "my-app",
										Env:  map[string]string{"FOO": "bar", "MY_SECRET": "s3cr3t"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestUpdateAppsEnv_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeAppsEnvFile(t, basicAppsEnvDoc("us10", "org1", "my-org"))

	_, _, err := runCmd(t, "update-apps-env", path, "-y")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("expected 'not logged in' error, got: %v", err)
	}
}

func TestUpdateAppsEnv_NoEnvInFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	doc := appsOutDoc{
		Regions: []appsOutRegion{{ID: "us10", Orgs: []appsOutOrg{{ID: "org1", Name: "my-org", Spaces: []appsOutSpace{
			{ID: "sp1", Name: "dev", Apps: []appsOutApp{{ID: "app1", Name: "my-app"}}},
		}}}}},
	}
	path := writeAppsEnvFile(t, doc)

	_, _, err := runCmd(t, "update-apps-env", path, "-y")
	if err == nil {
		t.Fatal("expected error for a file with no app_env entries")
	}
	if !strings.Contains(err.Error(), "no apps with environment variables") {
		t.Errorf("expected 'no apps with environment variables' error, got: %v", err)
	}
}

func TestUpdateAppsEnv_ConfirmAndUpdate(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/spaces":                          spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":                            appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/environment_variables": mustJSONStr(map[string]interface{}{"var": map[string]string{"FOO": "bar", "MY_SECRET": "s3cr3t"}}),
	})
	setupTestEnv(t, srv.URL)
	region := store.APIURLToRegion(srv.URL)

	path := writeAppsEnvFile(t, basicAppsEnvDoc(region, "org1", "my-org"))

	stdout, stderr, err := runCmd(t, "update-apps-env", path, "-y")
	if err != nil {
		t.Fatalf("update-apps-env failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "updated: ") || !strings.Contains(stdout, "my-app") {
		t.Errorf("expected an 'updated' line for my-app, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Done: 1 updated, 0 failed.") {
		t.Errorf("expected a final 1 updated/0 failed summary, got: %q", stdout)
	}
	// The sensitive key's value must never appear in plain text in the preview.
	if strings.Contains(stdout, "s3cr3t") {
		t.Errorf("expected MY_SECRET's value to be masked in the preview, got: %q", stdout)
	}
	if !strings.Contains(stdout, "MY_SECRET=********") {
		t.Errorf("expected MY_SECRET to be masked as ********, got: %q", stdout)
	}
	if !strings.Contains(stdout, "FOO=bar") {
		t.Errorf("expected non-sensitive FOO to be shown in plain text, got: %q", stdout)
	}
}

func TestUpdateAppsEnv_SkipsDeployAttributes(t *testing.T) {
	doc := basicAppsEnvDoc("us10", "org1", "my-org")
	doc.Regions[0].Orgs[0].Spaces[0].Apps[0].Env["DEPLOY_ATTRIBUTES"] = `{"foo":"bar"}`

	srv := fakeCFServer(t, map[string]string{
		"/v3/spaces":                          spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":                            appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/environment_variables": mustJSONStr(map[string]interface{}{"var": map[string]string{"FOO": "bar", "MY_SECRET": "s3cr3t"}}),
	})
	setupTestEnv(t, srv.URL)
	region := store.APIURLToRegion(srv.URL)
	doc.Regions[0].ID = region

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshaling fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "apps-env.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	stdout, stderr, err := runCmd(t, "update-apps-env", path, "-y")
	if err != nil {
		t.Fatalf("update-apps-env failed: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(stdout, "DEPLOY_ATTRIBUTES") {
		t.Errorf("expected DEPLOY_ATTRIBUTES to be excluded from the preview, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Done: 1 updated, 0 failed.") {
		t.Errorf("expected the remaining vars to still be updated, got: %q", stdout)
	}
}

func TestUpdateAppsEnv_AllEnvSkippedIsTreatedAsNoEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	doc := appsOutDoc{
		Regions: []appsOutRegion{{ID: "us10", Orgs: []appsOutOrg{{ID: "org1", Name: "my-org", Spaces: []appsOutSpace{
			{ID: "sp1", Name: "dev", Apps: []appsOutApp{
				{ID: "app1", Name: "my-app", Env: map[string]string{"DEPLOY_ATTRIBUTES": `{"foo":"bar"}`}},
			}},
		}}}}},
	}
	path := writeAppsEnvFile(t, doc)

	_, _, err := runCmd(t, "update-apps-env", path, "-y")
	if err == nil {
		t.Fatal("expected an error when every app's env is filtered out entirely")
	}
	if !strings.Contains(err.Error(), "no apps with environment variables") {
		t.Errorf("expected 'no apps with environment variables' error, got: %v", err)
	}
}

func TestUpdateAppsEnv_AbortsWithoutConfirmation(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/spaces": spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":   appsPageJSON("app1", "my-app", "sp1"),
	})
	setupTestEnv(t, srv.URL)
	region := store.APIURLToRegion(srv.URL)

	path := writeAppsEnvFile(t, basicAppsEnvDoc(region, "org1", "my-org"))

	// No -y, and stdin isn't 'y', so readLine should return a non-"y" answer
	// (or fail) and the command should abort without erroring.
	stdout, _, err := runCmd(t, "update-apps-env", path)
	if err != nil {
		t.Fatalf("update-apps-env failed: %v", err)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.' when confirmation is declined/unavailable, got: %q", stdout)
	}
}

func TestUpdateAppsEnv_AppNotFoundIsSkippedNotFatal(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/spaces": spacesPageJSON("sp1", "dev", "org1"),
		"/v3/apps":   emptyPage(), // no apps at all in this space
	})
	setupTestEnv(t, srv.URL)
	region := store.APIURLToRegion(srv.URL)

	path := writeAppsEnvFile(t, basicAppsEnvDoc(region, "org1", "my-org"))

	stdout, _, err := runCmd(t, "update-apps-env", path, "-y")
	if err != nil {
		t.Fatalf("update-apps-env failed: %v", err)
	}
	if !strings.Contains(stdout, "No matching apps found.") {
		t.Errorf("expected 'No matching apps found.' when the app can't be resolved, got: %q", stdout)
	}
}

func TestUpdateAppsEnv_RetargetsToDefaultOrgScope(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/spaces":                          spacesPageJSON("sp1", "dev", "org2"),
		"/v3/apps":                            appsPageJSON("app1", "my-app", "sp1"),
		"/v3/apps/app1/environment_variables": mustJSONStr(map[string]interface{}{"var": map[string]string{"FOO": "bar"}}),
	})
	setupTestEnv(t, srv.URL)
	region := store.APIURLToRegion(srv.URL)
	// The file says org1, but the default org scope (org2) must win.
	setDefaultOrgScope(t, srv.URL, "org2", "target-org")

	path := writeAppsEnvFile(t, basicAppsEnvDoc(region, "org1", "source-org"))

	stdout, stderr, err := runCmd(t, "update-apps-env", path, "-y")
	if err != nil {
		t.Fatalf("update-apps-env failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "target-org") {
		t.Errorf("expected retargeted org 'target-org' in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "source-org") {
		t.Errorf("file's own org 'source-org' should have been discarded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Note: targeting the org(s) currently selected via 'bo orgs'") {
		t.Errorf("expected the org-scope note in output, got: %q", stdout)
	}
}
