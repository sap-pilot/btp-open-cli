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

func TestUsers_Include(t *testing.T) {
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

	stdout, _, err := runCmd(t, "users", "--include", "alice", "--no-prompt")
	if err != nil {
		t.Fatalf("users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

func TestUsers_IncludeExcludeCSVKeywords(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "alice@example.com", "sap.ids"),
		xsuaaUser("u2", "bob@example.com", "uaa"),
		xsuaaUser("u3", "carol@example.com", "sap.default"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	// --include with two keywords: matches alice OR carol.
	stdout, _, err := runCmd(t, "users", "--include", "alice,carol", "--no-prompt")
	if err != nil {
		t.Fatalf("users --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") || !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected alice and carol in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be excluded by --include, got: %q", stdout)
	}

	// --exclude with two keywords: drops alice and bob.
	stdout, _, err = runCmd(t, "users", "--exclude", "alice,bob", "--no-prompt")
	if err != nil {
		t.Fatalf("users --exclude failed: %v", err)
	}
	if strings.Contains(stdout, "alice@example.com") || strings.Contains(stdout, "bob@example.com") {
		t.Errorf("alice and bob should be excluded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "carol@example.com") {
		t.Errorf("expected carol in output, got: %q", stdout)
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
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 CSV lines, got %d\nOutput: %s", len(lines), stdout)
	}
	if lines[0] != "user_name,user_origin" {
		t.Errorf("expected CSV header to be exactly the given --fields, got: %q", lines[0])
	}
	if lines[1] != "alice@example.com,sap.ids" {
		t.Errorf("expected data row to match --fields order, got: %q", lines[1])
	}
}

func TestUsers_SortBy(t *testing.T) {
	const orgGUID = "org1"
	xsuaaSrv := newXsuaaServer(t,
		xsuaaUser("u1", "bob@example.com", "uaa"),
		xsuaaUser("u2", "alice@example.com", "sap.ids"),
	)
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "users", "--format", "csv", "--fields", "email", "--sort-by", "email", "--no-prompt")
	if err != nil {
		t.Fatalf("users --sort-by failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || lines[1] != "alice@example.com" || lines[2] != "bob@example.com" {
		t.Errorf("expected alice before bob (ascending email sort), got: %v", lines)
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

func TestUsers_UARCSV_Include(t *testing.T) {
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

	stdout, _, err := runCmd(t, "users", "--format", "uar.csv", "--include", "alice", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format uar.csv --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "bob@example.com") {
		t.Errorf("bob should be filtered out, got: %q", stdout)
	}
}

// TestUsers_UARCSV_IncludeDropsUnmatchedNARows reproduces a bug where
// --include on the uar.csv format still emitted "N/A" placeholder rows for
// role collections whose only members were filtered out, or that never had
// any members, even though those rows never contain the include keyword.
func TestUsers_UARCSV_IncludeDropsUnmatchedNARows(t *testing.T) {
	const orgGUID = "org1"
	users := []map[string]interface{}{
		xsuaaUserWithGroups("u1", "alice@example.com", "sap.ids", xsuaaGroup("g1", "AFC_FULLACCESS")),
		xsuaaUserWithGroups("u2", "bob@example.com", "uaa", xsuaaGroup("g2", "Subaccount Viewer")),
	}
	// "Cloud Connector Administrator" has zero members at all; "Subaccount
	// Viewer" has a member (bob) who won't match --include "alice".
	xsuaaSrv := newXsuaaUsersAndRCServer(t, users, "AFC_FULLACCESS", "Subaccount Viewer", "Cloud Connector Administrator")
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, xsuaaSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	// Sanity check: without --include, both empty/unmatched role collections
	// produce N/A rows.
	stdout, _, err := runCmd(t, "users", "--format", "uar.csv", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format uar.csv failed: %v", err)
	}
	if !strings.Contains(stdout, "Cloud Connector Administrator,desc Cloud Connector Administrator,N/A,N/A") {
		t.Fatalf("expected an N/A row for Cloud Connector Administrator without --include, got: %q", stdout)
	}

	stdout, _, err = runCmd(t, "users", "--format", "uar.csv", "--include", "alice", "--no-prompt")
	if err != nil {
		t.Fatalf("users --format uar.csv --include failed: %v", err)
	}
	if !strings.Contains(stdout, "alice@example.com") {
		t.Errorf("expected alice's row in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "N/A") {
		t.Errorf("expected no N/A rows when --include doesn't match them, got: %q", stdout)
	}
	if strings.Contains(stdout, "Subaccount Viewer") {
		t.Errorf("Subaccount Viewer should be dropped entirely (its only member doesn't match --include), got: %q", stdout)
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
