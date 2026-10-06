package cmd

import (
	"strings"
	"testing"
)

func TestSpaces_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "spaces")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestSpaces_NoScopeNoFlags(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "spaces")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestSpaces_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "spaces")
	if err != nil {
		t.Fatalf("spaces command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-org") || !strings.Contains(stdout, "dev") {
		t.Errorf("expected org and space name in output, got: %q", stdout)
	}
}

func TestSpaces_JSON(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "spaces", "--format", "json")
	if err != nil {
		t.Fatalf("spaces --format json failed: %v", err)
	}
	if !strings.Contains(stdout, `"space_name": "dev"`) {
		t.Errorf("expected space_name in JSON output, got: %q", stdout)
	}
}

func TestSpaces_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "spaces", "--format", "csv")
	if err != nil {
		t.Fatalf("spaces --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines (header + data), got %d", len(lines))
	}
	if lines[0] != "region,org_id,org_name,space_id,space_name" {
		t.Errorf("unexpected CSV header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "dev") {
		t.Errorf("expected dev in CSV data row, got: %q", lines[1])
	}
}

func TestSpaces_OrgFlag(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
		"/v3/spaces": spacesPageJSON("sp1", "dev-in-org1", "org1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "spaces", "--org", "org1")
	if err != nil {
		t.Fatalf("spaces --org failed: %v", err)
	}
	if !strings.Contains(stdout, "org-one") {
		t.Errorf("expected org-one in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "org-two") {
		t.Errorf("org-two should not appear, got: %q", stdout)
	}
}

func TestSpaces_Include(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]interface{}{
				{"guid": "sp1", "name": "prod",
					"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
				{"guid": "sp2", "name": "dev",
					"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
			},
		}),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "spaces", "--include", "prod")
	if err != nil {
		t.Fatalf("spaces --include failed: %v", err)
	}
	if !strings.Contains(stdout, "prod") {
		t.Errorf("expected prod in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "dev") {
		t.Errorf("dev should be filtered out, got: %q", stdout)
	}
}

func TestSpaces_OutputFlag(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	outPath := t.TempDir() + "/spaces.csv"
	stdout, _, err := runCmd(t, "spaces", "--format", "csv", "--output", outPath)
	if err != nil {
		t.Fatalf("spaces --output failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no output on stdout when --output is set, got: %q", stdout)
	}
}
