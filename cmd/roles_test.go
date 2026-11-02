package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/xsuaa"
)

// rolesServerCalls records what newRolesAdminServer received.
type rolesServerCalls struct {
	mu            sync.Mutex
	created       []xsuaa.Role
	updated       []xsuaa.Role
	deletedNames  []string
	conflictNames map[string]bool // POST to one of these names returns 409
	notFoundNames map[string]bool // PUT/DELETE to one of these names returns 404
}

func (c *rolesServerCalls) recordCreate(r xsuaa.Role) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, r)
}

func (c *rolesServerCalls) recordUpdate(r xsuaa.Role) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updated = append(c.updated, r)
}

func (c *rolesServerCalls) recordDelete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedNames = append(c.deletedNames, name)
}

func (c *rolesServerCalls) isConflict(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conflictNames[name]
}

func (c *rolesServerCalls) isNotFound(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notFoundNames[name]
}

// newRolesAdminServer serves GET/POST/PUT on /sap/rest/authorization/v2/roles
// and GET/DELETE on its subpaths, plus an empty role-collections GET, backing
// the roles export/create/update/delete commands under test.
func newRolesAdminServer(t *testing.T, existingRoles []xsuaa.Role) (*httptest.Server, *rolesServerCalls) {
	t.Helper()
	calls := &rolesServerCalls{
		conflictNames: map[string]bool{},
		notFoundNames: map[string]bool{},
	}
	rolesGET := mustJSONStr(existingRoles)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sap/rest/authorization/v2/roles" && r.Method == http.MethodGet:
			w.Write([]byte(rolesGET)) //nolint:errcheck

		case r.URL.Path == "/sap/rest/authorization/v2/rolecollections" && r.Method == http.MethodGet:
			w.Write([]byte("[]")) //nolint:errcheck

		case r.URL.Path == "/sap/rest/authorization/v2/roles" && r.Method == http.MethodPost:
			var role xsuaa.Role
			json.NewDecoder(r.Body).Decode(&role) //nolint:errcheck
			calls.recordCreate(role)
			if calls.isConflict(role.Name) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)

		case r.URL.Path == "/sap/rest/authorization/v2/roles" && r.Method == http.MethodPut:
			var role xsuaa.Role
			json.NewDecoder(r.Body).Decode(&role) //nolint:errcheck
			calls.recordUpdate(role)
			if calls.isNotFound(role.Name) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/roles/") && r.Method == http.MethodDelete:
			name := strings.TrimPrefix(r.URL.Path, "/sap/rest/authorization/v2/roles/")
			calls.recordDelete(name)
			if calls.isNotFound(name) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestRoles_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "roles", "--no-prompt")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestRoles_Default(t *testing.T) {
	const orgGUID = "org1"
	rolesSrv, _ := newRolesAdminServer(t, []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "GlobalAdmin", Description: "d",
			AttributeList: []xsuaa.RoleAttribute{{AttributeName: "costcenter", AttributeValueList: []string{"1000"}}}},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rolesSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "roles", "--no-prompt")
	if err != nil {
		t.Fatalf("roles failed: %v", err)
	}
	if !strings.Contains(stdout, "GlobalAdmin") {
		t.Errorf("expected GlobalAdmin in output, got: %q", stdout)
	}
	if !strings.Contains(stdout, "costcenter") {
		t.Errorf("expected attributeList (costcenter) in output, got: %q", stdout)
	}
}

func TestRoles_JSON(t *testing.T) {
	const orgGUID = "org1"
	rolesSrv, _ := newRolesAdminServer(t, []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "GlobalAdmin"},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rolesSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "roles", "--format", "json", "--no-prompt")
	if err != nil {
		t.Fatalf("roles --format json failed: %v", err)
	}
	var doc roOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(doc.Regions) != 1 || len(doc.Regions[0].Orgs) != 1 || len(doc.Regions[0].Orgs[0].Roles) != 1 {
		t.Fatalf("unexpected doc shape: %+v", doc)
	}
}

// writeRolesFile writes a roOutDoc to a temp file in json or toon format and
// returns its path.
func writeRolesFile(t *testing.T, format string, doc roOutDoc) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "roles."+format)

	var data []byte
	var err error
	if format == "json" {
		data, err = json.Marshal(doc)
	} else {
		data, err = toonenc.Marshal(doc, toonenc.WithIndent(2))
	}
	if err != nil {
		t.Fatalf("encoding roles file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing roles file: %v", err)
	}
	return path
}

func testRolesDoc(orgGUID, orgName, region string) roOutDoc {
	return roOutDoc{Regions: []roOutRegion{{ID: region, Orgs: []roOutOrg{{ID: orgGUID, Name: orgName, Roles: []roOutRole{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "Custom Role A", Description: "d1",
			AttributeList: []roOutRoleAttr{{AttributeName: "costcenter", AttributeValueList: []string{"1000", "2000"}}}},
		{RoleTemplateAppID: "xsuaa!t2", RoleTemplateName: "xsuaa_viewer", Name: "Custom Role B", Description: "d2"},
	}}}}}}
}

func TestParseRolesFile_JSONAndTOON(t *testing.T) {
	doc := testRolesDoc("org1", "my-org", "us10")
	for _, format := range []string{"json", "toon"} {
		path := writeRolesFile(t, format, doc)
		entries, err := parseRolesFile(path)
		if err != nil {
			t.Fatalf("[%s] parseRolesFile failed: %v", format, err)
		}
		if len(entries) != 2 {
			t.Fatalf("[%s] expected 2 entries, got %d", format, len(entries))
		}
		if entries[0].Role.Name != "Custom Role A" || len(entries[0].Role.AttributeList) != 1 {
			t.Errorf("[%s] unexpected first entry: %+v", format, entries[0])
		}
	}
}

func TestCreateRoles_CreatesAndSkipsExisting(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	calls.conflictNames["Custom Role B"] = true
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)

	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-roles", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("create-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "+ [us10] my-org / Custom Role A") {
		t.Errorf("expected Custom Role A created, got: %q", stdout)
	}
	if !strings.Contains(stdout, "~ [us10] my-org / Custom Role B (already exists)") {
		t.Errorf("expected Custom Role B already-exists, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(calls.created))
	}
}

func TestUpdateRoles_UpdatesAndWarnsNotFound(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	calls.notFoundNames["Custom Role B"] = true
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)

	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "toon", doc)

	stdout, stderr, err := runCmd(t, "update-roles", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("update-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "~ [us10] my-org / Custom Role A") {
		t.Errorf("expected Custom Role A updated, got: %q", stdout)
	}
	if !strings.Contains(stderr, "Custom Role B: not found") {
		t.Errorf("expected Custom Role B not-found warning, got: %q", stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.updated) != 2 {
		t.Fatalf("expected 2 update calls, got %d", len(calls.updated))
	}
}

func TestDeleteRoles_FromFile(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)

	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "json", doc)

	// Pattern narrows the file's two roles down to just "Custom Role A".
	stdout, stderr, err := runCmd(t, "delete-roles", "*Role A", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("delete-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "- [us10] my-org / Custom Role A") {
		t.Errorf("expected Custom Role A deleted, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedNames) != 1 || calls.deletedNames[0] != "Custom Role A" {
		t.Errorf("expected only Custom Role A deleted, got: %v", calls.deletedNames)
	}
}

func TestDeleteRoles_Discovery(t *testing.T) {
	const orgGUID = "org1"
	rolesSrv, calls := newRolesAdminServer(t, []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "t1", Name: "custom-role"},
		{RoleTemplateAppID: "xsuaa!t2", RoleTemplateName: "t2", Name: "xsuaa_admin"},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rolesSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, stderr, err := runCmd(t, "delete-roles", "custom-*", "--yes", "--no-prompt")
	if err != nil {
		t.Fatalf("delete-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "custom-role") {
		t.Errorf("expected custom-role deleted, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedNames) != 1 || calls.deletedNames[0] != "custom-role" {
		t.Errorf("expected only custom-role deleted, got: %v", calls.deletedNames)
	}
}

func TestDeleteRoles_RequiresPatternWithoutRolesFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "delete-roles")
	if err == nil {
		t.Fatal("expected error when neither a pattern nor --roles is given")
	}
}
