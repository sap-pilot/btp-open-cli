package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		"id":            id,
		"externalId":    id + "-ext",
		"origin":        origin,
		"userName":      username,
		"emails":        []map[string]interface{}{{"value": username, "primary": true}},
		"lastLogonTime": 0,
		"groups":        []interface{}{},
	}
}

// xsuaaUserWithGroups returns a minimal SCIM user map with role-collection
// group memberships (SCIM groups map 1:1 to XSUAA role collections).
func xsuaaUserWithGroups(id, username, origin string, groups ...map[string]interface{}) map[string]interface{} {
	u := xsuaaUser(id, username, origin)
	groupList := make([]interface{}, len(groups))
	for i, g := range groups {
		groupList[i] = g
	}
	u["groups"] = groupList
	return u
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

// xsuaaGroup returns a SCIM group reference as embedded in a user's "groups".
func xsuaaGroup(id, displayName string) map[string]interface{} {
	return map[string]interface{}{"value": id, "display": displayName}
}

// newXsuaaUsersAndRCServer creates a fake XSUAA server serving both /Users
// and the role collections API, for uar.csv tests.
func newXsuaaUsersAndRCServer(t *testing.T, users []map[string]interface{}, rcNames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Users":
			w.Write([]byte(xsuaaUsersPage(users...))) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections"):
			w.Write([]byte(xsuaaRoleCollectionsPage(rcNames...))) //nolint:errcheck
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
		}
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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "users", "--format", "csv", "--fields", "user_name,user_origin", "--no-prompt")
	if err != nil {
		t.Fatalf("users --fields failed: %v", err)
	}
	// The CSV header is always all columns; --fields only blanks-out DATA row values.
	// Verify the data row: user_id (col 3) should be empty, user_name (col 6) should be set.
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
	// col 6 = user_name — must be populated
	if cols[6] != "alice@example.com" {
		t.Errorf("user_name (col 6) should be alice@example.com, got: %q", cols[6])
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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

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
	fs := buildUsrFieldSet("user_name,email", "")
	if !fs.active("user_name") {
		t.Error("user_name should be active")
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
	if !fs.active("user_name") {
		t.Error("user_name should still be active")
	}
}

func TestUsers_UARCSV(t *testing.T) {
	const orgGUID = "org1"
	users := []map[string]interface{}{
		xsuaaUserWithGroups("u1", "alice@example.com", "sap.ids",
			xsuaaGroup("g1", "AFC_FULLACCESS"), xsuaaGroup("g2", "Subaccount Viewer")),
		xsuaaUserWithGroups("u2", "bob@example.com", "uaa",
			xsuaaGroup("g1", "AFC_FULLACCESS")),
	}
	xsuaaSrv := newXsuaaUsersAndRCServer(t, users, "AFC_FULLACCESS", "Subaccount Viewer", "Cloud Connector Administrator")
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "users", "--format", "uar.csv", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format uar.csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if lines[0] != "Role Collection,Description,Role Collection Members,Origin,Subaccount ID" {
		t.Fatalf("unexpected header: %q", lines[0])
	}

	// Rows sorted by Role Collection name: AFC_FULLACCESS (2 members) comes
	// before Cloud Connector Administrator (no members), which comes before
	// Subaccount Viewer (1 member).
	want := []string{
		"AFC_FULLACCESS,desc AFC_FULLACCESS,alice@example.com,sap.ids," + orgGUID,
		"AFC_FULLACCESS,desc AFC_FULLACCESS,bob@example.com,uaa," + orgGUID,
		"Cloud Connector Administrator,desc Cloud Connector Administrator,N/A,N/A," + orgGUID,
		"Subaccount Viewer,desc Subaccount Viewer,alice@example.com,sap.ids," + orgGUID,
	}
	got := lines[1:]
	if len(got) != len(want) {
		t.Fatalf("expected %d data rows, got %d:\n%s", len(want), len(got), stdout)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d: expected %q, got %q", i, w, got[i])
		}
	}
}

func TestUsers_UARCSV_Filter(t *testing.T) {
	const orgGUID = "org1"
	users := []map[string]interface{}{
		xsuaaUserWithGroups("u1", "alice@example.com", "sap.ids", xsuaaGroup("g1", "AFC_FULLACCESS")),
		xsuaaUserWithGroups("u2", "bob@example.com", "uaa", xsuaaGroup("g1", "AFC_FULLACCESS")),
	}
	xsuaaSrv := newXsuaaUsersAndRCServer(t, users, "AFC_FULLACCESS")
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "users", "--format", "uar.csv", "--filter", "alice", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format uar.csv --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

func TestUsers_OutputFlag(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	outPath := filepath.Join(t.TempDir(), "users.csv")
	stdout, _, err := runCmd(t, "users", "--format", "csv", "--output", outPath, "--no-prompt")
	if err != nil {
		t.Fatalf("users --output failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no result on stdout when --output is set, got: %q", stdout)
	}
	data, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("reading output file: %v", readErr)
	}
	if !strings.Contains(string(data), "alice@example.com") {
		t.Errorf("expected alice in output file, got: %q", string(data))
	}
}
