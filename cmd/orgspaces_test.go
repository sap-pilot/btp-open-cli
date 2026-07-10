package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// spacesPageJSON returns a CF v3 spaces response with one space in the given org.
func spacesPageJSON(spaceGUID, spaceName, orgGUID string) string {
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]interface{}{
			{
				"guid": spaceGUID,
				"name": spaceName,
				"relationships": map[string]interface{}{
					"organization": map[string]interface{}{
						"data": map[string]string{"guid": orgGUID},
					},
				},
			},
		},
	})
}

func TestOrgSpaces_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "org-spaces")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestOrgSpaces_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces")
	if err != nil {
		t.Fatalf("org-spaces command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-org") {
		t.Errorf("expected my-org in output, got: %q", stdout)
	}
	if !strings.Contains(stdout, "dev") {
		t.Errorf("expected space name 'dev' in output, got: %q", stdout)
	}
}

func TestOrgSpaces_JSON(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces", "--format", "json")
	if err != nil {
		t.Fatalf("org-spaces --format json failed: %v", err)
	}

	var doc osOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, stdout)
	}
	if len(doc.Regions) == 0 {
		t.Fatal("expected at least one region")
	}
	if len(doc.Regions[0].Orgs) == 0 {
		t.Fatal("expected at least one org")
	}
	if len(doc.Regions[0].Orgs[0].Spaces) == 0 {
		t.Fatal("expected at least one space")
	}
	if doc.Regions[0].Orgs[0].Spaces[0].Name != "dev" {
		t.Errorf("expected space 'dev', got %q", doc.Regions[0].Orgs[0].Spaces[0].Name)
	}
}

// TestOrgSpaces_IncludeByOrgName verifies that --include matching org_name keeps
// all spaces of that org and drops orgs that don't match.
func TestOrgSpaces_IncludeByOrgName(t *testing.T) {
	twoOrgs := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
		"resources": []map[string]string{
			{"guid": "org1", "name": "prod-org"},
			{"guid": "org2", "name": "dev-org"},
		},
	})
	twoSpaces := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
		"resources": []map[string]interface{}{
			{"guid": "sp1", "name": "dev",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
			{"guid": "sp2", "name": "staging",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org2"}}}},
		},
	})
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": twoOrgs,
		"/v3/spaces":        twoSpaces,
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces", "--include", "prod")
	if err != nil {
		t.Fatalf("org-spaces --include (org_name) failed: %v", err)
	}
	if !strings.Contains(stdout, "prod-org") {
		t.Errorf("expected prod-org in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "dev-org") {
		t.Errorf("dev-org should be excluded by --include, got: %q", stdout)
	}
}

// TestOrgSpaces_IncludeBySpaceName verifies that --include matching space_name
// keeps only the matching spaces (from any org).
func TestOrgSpaces_IncludeBySpaceName(t *testing.T) {
	twoSpaces := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
		"resources": []map[string]interface{}{
			{"guid": "sp1", "name": "prod",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
			{"guid": "sp2", "name": "dev",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
		},
	})
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        twoSpaces,
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces", "--include", "prod")
	if err != nil {
		t.Fatalf("org-spaces --include (space_name) failed: %v", err)
	}
	if !strings.Contains(stdout, "prod") {
		t.Errorf("expected prod space in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "dev") {
		t.Errorf("dev space should be excluded by --include, got: %q", stdout)
	}
}

// TestOrgSpaces_ExcludeBySpaceName verifies that --exclude matching space_name
// removes matching spaces from output.
func TestOrgSpaces_ExcludeBySpaceName(t *testing.T) {
	twoSpaces := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
		"resources": []map[string]interface{}{
			{"guid": "sp1", "name": "prod",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
			{"guid": "sp2", "name": "dev",
				"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
		},
	})
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        twoSpaces,
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces", "--exclude", "prod")
	if err != nil {
		t.Fatalf("org-spaces --exclude (space_name) failed: %v", err)
	}
	if strings.Contains(stdout, "prod") {
		t.Errorf("prod space should be excluded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "dev") {
		t.Errorf("expected dev space in output, got: %q", stdout)
	}
}

func TestOrgSpaces_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/spaces":        spacesPageJSON("sp1", "dev", "org1"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-spaces", "--format", "csv")
	if err != nil {
		t.Fatalf("org-spaces --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}
	if lines[0] != "region,org_id,org_name,space_id,space_name" {
		t.Errorf("unexpected CSV header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "dev") {
		t.Errorf("expected 'dev' in CSV data, got: %q", lines[1])
	}
}
