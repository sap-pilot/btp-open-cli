package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// xsuaaUsersPage returns a SCIM /Users response with the given users.
func xsuaaUsersPage(users ...map[string]interface{}) string {
	return mustJSONStr(map[string]interface{}{
		"totalResults": len(users),
		"startIndex":   1,
		"itemsPerPage": 500,
		"Resources":    users,
	})
}

// xsuaaUser returns a minimal SCIM user map.
func xsuaaUser(id, username, origin string) map[string]interface{} {
	return map[string]interface{}{
		"id":           id,
		"externalId":   id + "-ext",
		"origin":       origin,
		"userName":     username,
		"emails":       []map[string]interface{}{{"value": username, "primary": true}},
		"lastLogonTime": 0,
		"groups":       []interface{}{},
	}
}

// newXsuaaServer creates a fake XSUAA SCIM server returning the given users.
func newXsuaaServer(t *testing.T, users ...map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(xsuaaUsersPage(users...))) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUsers_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "users", "--no-prompt")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestUsers_DefaultToon(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	stdout, _, err := runCmd(t, "users", "--no-prompt")
	if err != nil {
		t.Fatalf("users command failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in output, got: %q", stdout)
	}
}

func TestUsers_JSON(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	stdout, _, err := runCmd(t, "users", "--format", "json", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format json failed: %v", err)
	}
	var doc usrOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, stdout)
	}
	if len(doc.Regions) == 0 || len(doc.Regions[0].Orgs) == 0 {
		t.Fatal("expected at least one region and org")
	}
}

func TestUsers_CSV(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	stdout, _, err := runCmd(t, "users", "--format", "csv", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "user_id") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
}

func TestUsers_Filter(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
		xsuaaUser("u2", "bob@example.com", "uaa"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	stdout, _, err := runCmd(t, "users", "--filter", "alice", "--no-prompt")
	if err != nil {
		t.Fatalf("users --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

func TestUsers_Fields(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	stdout, _, err := runCmd(t, "users", "--format", "csv", "--fields", "userName,user_origin", "--no-prompt")
	if err != nil {
		t.Fatalf("users --fields failed: %v", err)
	}
	// The CSV header is always all columns; --fields only blanks-out DATA row values.
	// Verify the data row: user_id (col 3) should be empty, userName (col 6) should be set.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 CSV lines, got %d\nOutput: %s", len(lines), stdout)
	}
	cols := strings.Split(lines[1], ",")
	if len(cols) < 7 {
		t.Fatalf("expected at least 7 CSV columns in data row, got %d: %q", len(cols), lines[1])
	}
	// col 3 = user_id — must be blank when not in --fields
	if cols[3] != "" {
		t.Errorf("user_id (col 3) should be blank when not in --fields, got: %q", cols[3])
	}
	// col 6 = userName — must be populated
	if cols[6] != "alice@example.com" {
		t.Errorf("userName (col 6) should be alice@example.com, got: %q", cols[6])
	}
}

func TestUsers_ExcludeFields(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)

	// Exclude user_id and user_externalId — both have non-empty values so the
	// absence is unambiguous (unlike lastLogonTime which is "" when zero anyway).
	stdout, _, err := runCmd(t, "users", "--format", "csv", "--excludeFields", "user_id,user_externalId", "--no-prompt")
	if err != nil {
		t.Fatalf("users --excludeFields failed: %v", err)
	}
	// The CSV header always lists all columns; check DATA row values instead.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 CSV lines, got %d\nOutput: %s", len(lines), stdout)
	}
	cols := strings.Split(lines[1], ",")
	if len(cols) < 7 {
		t.Fatalf("expected at least 7 CSV columns in data row, got %d: %q", len(cols), lines[1])
	}
	// col 3 = user_id — must be blank when excluded
	if cols[3] != "" {
		t.Errorf("user_id (col 3) should be blank when excluded, got: %q", cols[3])
	}
	// col 4 = user_externalId — must be blank when excluded
	if cols[4] != "" {
		t.Errorf("user_externalId (col 4) should be blank when excluded, got: %q", cols[4])
	}
	// col 6 = userName — must still be populated
	if cols[6] != "alice@example.com" {
		t.Errorf("userName (col 6) should be alice@example.com, got: %q", cols[6])
	}
}

// TestUsrFieldSet_All verifies that nil fieldset includes all fields.
func TestUsrFieldSet_All(t *testing.T) {
	fs := buildUsrFieldSet("", "")
	if fs != nil {
		t.Error("expected nil fieldset for no args (all fields active)")
	}
}

// TestUsrFieldSet_Include verifies that --fields limits the active set.
func TestUsrFieldSet_Include(t *testing.T) {
	fs := buildUsrFieldSet("userName,email", "")
	if !fs.active("userName") {
		t.Error("userName should be active")
	}
	if fs.active("user_id") {
		t.Error("user_id should not be active")
	}
}

// TestUsrFieldSet_Exclude verifies that --excludeFields removes from default set.
func TestUsrFieldSet_Exclude(t *testing.T) {
	fs := buildUsrFieldSet("", "lastLogonTime")
	if fs.active("lastLogonTime") {
		t.Error("lastLogonTime should not be active after exclusion")
	}
	if !fs.active("userName") {
		t.Error("userName should still be active")
	}
}
