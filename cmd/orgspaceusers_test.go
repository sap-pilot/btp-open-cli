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
		"/v3/spaces/sp1/users":         orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-space-users", "--format", "csv")
	if err != nil {
		t.Fatalf("org-space-users --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "cfuser_id") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
}
