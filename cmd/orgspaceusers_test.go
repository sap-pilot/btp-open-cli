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
		"/v3/spaces": spacesPageJSON("sp1", "dev", "org1"),
		"/v3/spaces/sp1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":            emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-space-users")
	if err != nil {
		t.Fatalf("org-space-users command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-org") {
		t.Errorf("expected org in output, got: %q", stdout)
	}
}

func TestOrgSpaceUsers_Filter(t *testing.T) {
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

	stdout, _, err := runCmd(t, "org-space-users", "--filter", "alice")
	if err != nil {
		t.Fatalf("org-space-users --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
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
