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
	conflictNames map[string]bool      // POST to one of these names returns 409
	notFoundNames map[string]bool      // PUT/DELETE to one of these names returns 404
	roleTemplates []xsuaa.RoleTemplate // GET .../apps/roletemplates returns this; set directly before running a command
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

// roleNameFromRolesByTemplatePath extracts {roleName} (the last path
// segment) from
// /sap/rest/authorization/v2/apps/{appId}/roletemplates/{templateName}/roles/{roleName}.
func roleNameFromRolesByTemplatePath(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// newRolesAdminServer serves GET on /sap/rest/authorization/v2/roles (and an
// empty role-collections GET), POST .../apps/roles to create, and PUT/DELETE
// on .../apps/{appId}/roletemplates/{templateName}/roles/{roleName} to
// update/delete — per the XSUAA Authorization API spec (tmp/api/AuthorizationAPI.yaml)
// — backing the roles export/create/update/delete commands under test. Per
// that spec, DELETE returns HTTP 200 when the role was deleted and HTTP 204
// when it didn't exist (inverted from the usual convention).
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

		case r.URL.Path == "/sap/rest/authorization/v2/apps/roletemplates" && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(calls.roleTemplates))) //nolint:errcheck

		case r.URL.Path == "/sap/rest/authorization/v2/apps/roles" && r.Method == http.MethodPost:
			var role xsuaa.Role
			json.NewDecoder(r.Body).Decode(&role) //nolint:errcheck
			calls.recordCreate(role)
			if calls.isConflict(role.Name) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)

		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/apps/") && strings.Contains(r.URL.Path, "/roletemplates/") && r.Method == http.MethodPut:
			name := roleNameFromRolesByTemplatePath(r.URL.Path)
			var body struct {
				AttributeList []xsuaa.RoleAttribute `json:"attributeList"`
				Description   string                `json:"description"`
			}
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			calls.recordUpdate(xsuaa.Role{Name: name, Description: body.Description, AttributeList: body.AttributeList})
			if calls.isNotFound(name) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/apps/") && strings.Contains(r.URL.Path, "/roletemplates/") && r.Method == http.MethodDelete:
			name := roleNameFromRolesByTemplatePath(r.URL.Path)
			calls.recordDelete(name)
			if calls.isNotFound(name) {
				w.WriteHeader(http.StatusNoContent) // per the API spec, 204 means "the role doesn't exist"
				return
			}
			w.WriteHeader(http.StatusOK) // per the API spec, 200 means "deleted"

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
			AttributeList: []xsuaa.RoleAttribute{{AttributeName: "costcenter", AttributeValues: []string{"1000"}}}},
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

func TestRoles_NamePatternFilters(t *testing.T) {
	const orgGUID = "org1"
	rolesSrv, _ := newRolesAdminServer(t, []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "GlobalAdmin"},
		{RoleTemplateAppID: "xsuaa!t2", RoleTemplateName: "xsuaa_viewer", Name: "Custom Viewer"},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rolesSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, _, err := runCmd(t, "roles", "Custom*", "--no-prompt")
	if err != nil {
		t.Fatalf("roles failed: %v", err)
	}
	if !strings.Contains(stdout, "Custom Viewer") {
		t.Errorf("expected Custom Viewer in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "GlobalAdmin") {
		t.Errorf("expected GlobalAdmin to be excluded by the pattern, got: %q", stdout)
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
			AttributeList: []roOutRoleAttr{{AttributeName: "costcenter", AttributeValues: []string{"1000", "2000"}}}},
		{RoleTemplateAppID: "xsuaa!t2", RoleTemplateName: "xsuaa_viewer", Name: "Custom Role B", Description: "d2"},
	}}}}}}
}

func TestRoles_OutputJSONExtensionAutoDetected(t *testing.T) {
	const orgGUID = "org1"
	rolesSrv, _ := newRolesAdminServer(t, []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "GlobalAdmin"},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rolesSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	dir := t.TempDir()
	path := filepath.Join(dir, "roles.json")
	_, _, err := runCmd(t, "roles", "--output", path, "--no-prompt")
	if err != nil {
		t.Fatalf("roles --output roles.json failed: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	var doc roOutDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("expected JSON auto-detected from .json extension, got invalid JSON: %v\n%s", err, body)
	}
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

func TestCreateRoles_OrgScopeMismatch_YesFlagRetargetsToCurrentScope(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	// The file's only org (us20/org2) differs from the current 'bo orgs'
	// scope (us10/org1) — -y must retarget both roles onto org1, discarding
	// the file's own org entirely (not filter them out).
	doc := roOutDoc{Regions: []roOutRegion{
		{ID: "us20", Orgs: []roOutOrg{{ID: "org2", Name: "other-org", Roles: []roOutRole{
			{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "t1", Name: "Role A"},
			{RoleTemplateAppID: "xsuaa!t2", RoleTemplateName: "t2", Name: "Role B"},
		}}}},
	}}
	path := writeRolesFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-roles", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("create-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "[us10] my-org / Role A") || !strings.Contains(stdout, "[us10] my-org / Role B") {
		t.Errorf("expected both roles retargeted onto the current scope (us10/my-org), got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 2 {
		t.Errorf("expected both roles created, got: %+v", calls.created)
	}
}

func TestCreateRoles_OrgScopeMatchesCurrentScope_NoRetargeting(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	// The file's org already matches the current 'bo orgs' scope exactly —
	// no prompt, no retargeting needed.
	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "json", doc)

	_, stderr, err := runCmd(t, "create-roles", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("create-roles failed: %v\nstderr: %s", err, stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 2 {
		t.Errorf("expected both roles created, got: %+v", calls.created)
	}
}

func TestCreateRoles_ScopeNoteShownWhenDefaultOrgScopeSelected(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "json", doc)

	// No -y: the preview (shown before the main confirm) must note that
	// the 'bo orgs' selection is being targeted, and how to redirect it.
	// Stdin hits EOF in tests, so the main confirm then aborts safely.
	stdout, _, err := runCmd(t, "create-roles", "--roles", path)
	if err != nil {
		t.Fatalf("create-roles failed: %v", err)
	}
	if !strings.Contains(stdout, "targeting the org(s) currently selected via 'bo orgs'") {
		t.Errorf("expected the org-scope note in the preview, got: %q", stdout)
	}
	if !strings.Contains(stdout, "us10 / my-org") {
		t.Errorf("expected the note to name the targeted org, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected EOF on the main confirm to abort cleanly, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 0 {
		t.Errorf("expected no roles created (aborted), got: %+v", calls.created)
	}
}

func TestCreateRoles_ResolvesRoleTemplateAppIDAcrossSubaccounts(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	// The target subaccount has the same app/template, but under a
	// different appId suffix than the one the role was exported with.
	calls.roleTemplates = []xsuaa.RoleTemplate{
		{AppID: "btp-dummy!t999999", AppName: "btp-dummy", Name: "BTP_Dummy_Admin"},
	}
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	doc := roOutDoc{Regions: []roOutRegion{
		{ID: "us20", Orgs: []roOutOrg{{ID: "org2", Name: "other-org", Roles: []roOutRole{
			{RoleTemplateAppID: "btp-dummy!t147007", RoleTemplateName: "BTP_Dummy_Admin", Name: "BTP_Dummy_CBU"},
		}}}},
	}}
	path := writeRolesFile(t, "json", doc)

	// -y retargets the role onto the current scope (a different subaccount
	// than it was exported from), so its exported appId suffix (!t147007)
	// must be re-resolved to the one actually registered there (!t999999).
	_, stderr, err := runCmd(t, "create-roles", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("create-roles failed: %v\nstderr: %s", err, stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls.created))
	}
	if calls.created[0].RoleTemplateAppID != "btp-dummy!t999999" {
		t.Errorf("expected roleTemplateAppId resolved to btp-dummy!t999999, got: %q", calls.created[0].RoleTemplateAppID)
	}
}

func TestCreateRoles_PatternFiltersFileEntries(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, calls := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)

	doc := testRolesDoc(orgGUID, orgName, region) // "Custom Role A" and "Custom Role B"
	path := writeRolesFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-roles", "*Role A", "--roles", path, "--yes")
	if err != nil {
		t.Fatalf("create-roles failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "+ [us10] my-org / Custom Role A") {
		t.Errorf("expected Custom Role A created, got: %q", stdout)
	}
	if strings.Contains(stdout, "Custom Role B") {
		t.Errorf("expected Custom Role B to be excluded by the pattern, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0].Name != "Custom Role A" {
		t.Errorf("expected only Custom Role A created, got: %+v", calls.created)
	}
}

func TestCreateRoles_PatternMatchesNothing(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rolesSrv, _ := newRolesAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rolesSrv.URL)

	doc := testRolesDoc(orgGUID, orgName, region)
	path := writeRolesFile(t, "json", doc)

	_, _, err := runCmd(t, "create-roles", "nonexistent-*", "--roles", path, "--yes")
	if err == nil {
		t.Fatal("expected error when the pattern matches no role in the file")
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
