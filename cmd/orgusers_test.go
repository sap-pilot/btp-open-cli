package cmd

import (
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

// orgUsersPage returns a CF v3 /v3/organizations/{id}/users response.
func orgUsersPage(users ...map[string]string) string {
	resources := make([]map[string]string, len(users))
	copy(resources, users)
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources":  resources,
	})
}

// cfUser returns a user map for use in org/space user responses.
func cfUser(guid, username, origin string) map[string]string {
	return map[string]string{"guid": guid, "username": username, "origin": origin}
}

func TestOrgUsers_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "org-users")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestOrgUsers_NoScopeNoFlags(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/roles":         emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "org-users")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestOrgUsers_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-users")
	if err != nil {
		t.Fatalf("org-users command failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected user in output, got: %q", stdout)
	}
}

func TestOrgUsers_Include(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(
			cfUser("u1", "alice@example.com", "sap.ids"),
			cfUser("u2", "bob@example.com", "uaa"),
		),
		"/v3/roles": emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-users", "--include", "alice")
	if err != nil {
		t.Fatalf("org-users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

func TestOrgUsers_IncludeExcludeCSVKeywords(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(
			cfUser("u1", "alice@example.com", "sap.ids"),
			cfUser("u2", "bob@example.com", "uaa"),
			cfUser("u3", "carol@example.com", "sap.default"),
		),
		"/v3/roles": emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-users", "--include", "alice,carol")
	if err != nil {
		t.Fatalf("org-users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") || !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected alice and carol in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be excluded by --include, got: %q", stdout)
	}

	stdout, _, err = runCmd(t, "org-users", "--exclude", "alice,bob")
	if err != nil {
		t.Fatalf("org-users --exclude failed: %v", err)
	}
	if strings.Contains(stdout, "alice@example.com") || strings.Contains(stdout, "bob@example.com") {
		t.Errorf("alice and bob should be excluded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected carol in output, got: %q", stdout)
	}
}

func TestOrgUsers_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations":            singleOrgPage("org1", "my-org"),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "org-users", "--format", "csv")
	if err != nil {
		t.Fatalf("org-users --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "cfuser_id") {
		t.Errorf("expected CSV header with cfuser_id, got: %q", lines[0])
	}
	if !strings.Contains(lines[1], "alice@example.com") {
		t.Errorf("expected alice in CSV data, got: %q", lines[1])
	}
}

func TestOrgUsers_OrgFilter(t *testing.T) {
	// Only org1 should be queried; org2 should be skipped.
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "org-users", "--org", "org1")
	if err != nil {
		t.Fatalf("org-users --org failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in output, got: %q", stdout)
	}
	// org-two should not appear
	if strings.Contains(stdout, "org-two") {
		t.Errorf("org-two should be filtered out, got: %q", stdout)
	}
}

func TestOrgUsers_DefaultScope_MultipleOrgs(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": mustJSONStr(map[string]interface{}{
			"pagination": map[string]interface{}{"total_pages": 1},
			"resources": []map[string]string{
				{"guid": "org1", "name": "org-one"},
				{"guid": "org2", "name": "org-two"},
			},
		}),
		"/v3/organizations/org1/users": orgUsersPage(cfUser("u1", "alice@example.com", "sap.ids")),
		"/v3/organizations/org2/users": orgUsersPage(cfUser("u2", "bob@example.com", "sap.ids")),
		"/v3/roles":                    emptyPage(),
	})
	setupTestEnv(t, srv.URL)
	creds, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	creds.DefaultOrgScope = []store.OrgScopeRef{
		{Region: store.APIURLToRegion(srv.URL), ID: "org1", Name: "org-one", APIURL: srv.URL},
		{Region: store.APIURLToRegion(srv.URL), ID: "org2", Name: "org-two", APIURL: srv.URL},
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "org-users")
	if err != nil {
		t.Fatalf("org-users with multi-org default scope failed: %v", err)
	}
	if !strings.Contains(stdout, "org-one") || !strings.Contains(stdout, "org-two") {
		t.Errorf("expected both orgs from default scope, got: %q", stdout)
	}
}
