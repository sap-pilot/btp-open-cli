package cmd

import (
	"strings"
	"testing"
)

func TestOrgSpaceUsers_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "org-space-users")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestOrgSpaceUsers_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/spaces":                   spacesPageJSON("sp1", "dev", "org1"),
		"/v3/spaces/sp1/users":         orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-space-users")
	if err != nil {
		t.Fatalf("org-space-users command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-org") {
		t.Errorf("expected org in output, got: %q", stdout)
	}
}

func TestOrgSpaceUsers_Include(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/spaces":                   spacesPageJSON("sp1", "dev", "org1"),
		"/v3/spaces/sp1/users": orgUsersPage(
			cfUser("u1", "alice@example.com", "sap.ids"),
			cfUser("u2", "bob@example.com", "uaa"),
		),
		"/v3/roles": emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-space-users", "--include", "alice")
	if err != nil {
		t.Fatalf("org-space-users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

func TestOrgSpaceUsers_IncludeExcludeCSVKeywords(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/spaces":                   spacesPageJSON("sp1", "dev", "org1"),
		"/v3/spaces/sp1/users": orgUsersPage(
			cfUser("u2", "bob@example.com", "uaa"),
			cfUser("u3", "carol@example.com", "sap.default"),
		),
		"/v3/roles": emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-space-users", "--include", "alice,carol")
	if err != nil {
		t.Fatalf("org-space-users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") || !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected alice and carol in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be excluded by --include, got: %q", stdout)
	}

	stdout, _, err = runCmd(t, "org-space-users", "--exclude", "alice,bob")
	if err != nil {
		t.Fatalf("org-space-users --exclude failed: %v", err)
	}
	if strings.Contains(stdout, "alice@example.com") || strings.Contains(stdout, "bob@example.com") {
		t.Errorf("alice and bob should be excluded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected carol in output, got: %q", stdout)
	}
}

func TestOrgSpaceUsers_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/spaces":                   spacesPageJSON("sp1", "dev", "org1"),
		"/v3/spaces/sp1/users":         orgUsersPage(cfUser("u2", "bob@example.com", "uaa")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-space-users", "--format", "csv")
	if err != nil {
		t.Fatalf("org-space-users --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected header + at least 2 data rows, got %d lines:\n%s", len(lines), stdout)
	}
	// Header must use space_id/space_name (no scope column).
	wantHeader := "region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles"
	if lines[0] != wantHeader {
		t.Errorf("unexpected CSV header:\n got:  %q\n want: %q", lines[0], wantHeader)
	}
	// Org-level row (alice): space_id and space_name must be empty.
	orgRow := strings.Split(lines[1], ",")
	if len(orgRow) < 5 {
		t.Fatalf("org row has too few columns: %q", lines[1])
	}
	if orgRow[3] != "" || orgRow[4] != "" {
		t.Errorf("org-level row should have empty space_id/space_name, got space_id=%q space_name=%q", orgRow[3], orgRow[4])
	}
	if !strings.Contains(lines[1], "alice@example.com") {
		t.Errorf("expected alice in org-level row, got: %q", lines[1])
	}
	// Space-level row (bob): space_id and space_name must be populated.
	spaceRow := strings.Split(lines[2], ",")
	if len(spaceRow) < 5 {
		t.Fatalf("space row has too few columns: %q", lines[2])
	}
	if spaceRow[3] == "" || spaceRow[4] == "" {
		t.Errorf("space-level row should have non-empty space_id/space_name, got space_id=%q space_name=%q", spaceRow[3], spaceRow[4])
	}
	if !strings.Contains(lines[2], "bob@example.com") {
		t.Errorf("expected bob in space-level row, got: %q", lines[2])
	}
}

func TestOrgSpaceUsers_UARCSV(t *testing.T) {
	rolesPage := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]interface{}{
			{"guid": "role1", "type": "organization_manager",
				"relationships": map[string]interface{}{
					"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
					"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
					"space":        map[string]interface{}{"data": nil},
				}},
			{"guid": "role2", "type": "organization_user",
				"relationships": map[string]interface{}{
					"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
					"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
					"space":        map[string]interface{}{"data": nil},
				}},
			{"guid": "role3", "type": "space_developer",
				"relationships": map[string]interface{}{
					"user":         map[string]interface{}{"data": map[string]string{"guid": "u2"}},
					"organization": map[string]interface{}{"data": nil},
					"space":        map[string]interface{}{"data": map[string]string{"guid": "sp1"}},
				}},
		},
	})
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "cvx-afc-prod"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "laura.castro@chevron.com", "sap.ids")),
		"/v3/spaces":                   spacesPageJSON("sp1", "dcore", "org1"),
		"/v3/spaces/sp1/users":         orgUsersPage(cfUser("u2", "firc@chevron.com", "sap.ids")),
		"/v3/roles":                    rolesPage,
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "cvx-afc-prod")

	stdout, _, err := runCmd(t, "org-space-users", "--format", "uar.csv")
	if err != nil {
		t.Fatalf("org-space-users --format uar.csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected header + at least 2 data rows, got %d lines:\n%s", len(lines), stdout)
	}
	wantHeader := "Space/Org ID,Space/Org Name,Group Type,Member,Role"
	if lines[0] != wantHeader {
		t.Errorf("unexpected CSV header:\n got:  %q\n want: %q", lines[0], wantHeader)
	}
	wantOrgRow := `org1,cvx-afc-prod,Organization,laura.castro@chevron.com,"organization_manager, organization_user"`
	if lines[1] != wantOrgRow {
		t.Errorf("unexpected org row:\n got:  %q\n want: %q", lines[1], wantOrgRow)
	}
	wantSpaceRow := "sp1,dcore,Space,firc@chevron.com,space_developer"
	if lines[2] != wantSpaceRow {
		t.Errorf("unexpected space row:\n got:  %q\n want: %q", lines[2], wantSpaceRow)
	}
}
