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

// rcAdminServerCalls records what newRCAdminServer received.
type rcAdminServerCalls struct {
	mu            sync.Mutex
	created       []xsuaa.RoleCollection
	updated       []xsuaa.RoleCollection
	deletedNames  []string
	conflictNames map[string]bool // POST to one of these names returns 409
	notFoundNames map[string]bool // PUT/DELETE to one of these names returns 404
}

func (c *rcAdminServerCalls) recordCreate(rc xsuaa.RoleCollection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, rc)
}

func (c *rcAdminServerCalls) recordUpdate(rc xsuaa.RoleCollection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updated = append(c.updated, rc)
}

func (c *rcAdminServerCalls) recordDelete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedNames = append(c.deletedNames, name)
}

func (c *rcAdminServerCalls) isConflict(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conflictNames[name]
}

func (c *rcAdminServerCalls) isNotFound(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notFoundNames[name]
}

// newRCAdminServer serves GET on /sap/rest/authorization/v2/rolecollections
// (and roles), POST to create, and PUT/DELETE on name subpaths, backing the
// role-collections export/create/update/delete commands under test.
func newRCAdminServer(t *testing.T, existingRCs []xsuaa.RoleCollection) (*httptest.Server, *rcAdminServerCalls) {
	t.Helper()
	calls := &rcAdminServerCalls{
		conflictNames: map[string]bool{},
		notFoundNames: map[string]bool{},
	}
	rcsGET := mustJSONStr(existingRCs)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sap/rest/authorization/v2/roles" && r.Method == http.MethodGet:
			w.Write([]byte("[]")) //nolint:errcheck

		case r.URL.Path == "/sap/rest/authorization/v2/rolecollections" && r.Method == http.MethodGet:
			w.Write([]byte(rcsGET)) //nolint:errcheck

		case r.URL.Path == "/sap/rest/authorization/v2/rolecollections" && r.Method == http.MethodPost:
			var rc xsuaa.RoleCollection
			json.NewDecoder(r.Body).Decode(&rc) //nolint:errcheck
			calls.recordCreate(rc)
			if calls.isConflict(rc.Name) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)

		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections/") && r.Method == http.MethodPut:
			name := strings.TrimPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections/")
			var rc xsuaa.RoleCollection
			json.NewDecoder(r.Body).Decode(&rc) //nolint:errcheck
			calls.recordUpdate(rc)
			if calls.isNotFound(name) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections/") && r.Method == http.MethodDelete:
			name := strings.TrimPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections/")
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

// writeRCFile writes an rcOutDoc to a temp file in json or toon format and
// returns its path.
func writeRCFile(t *testing.T, format string, doc rcOutDoc) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "role-collections."+format)

	var data []byte
	var err error
	if format == "json" {
		data, err = json.Marshal(doc)
	} else {
		data, err = toonenc.Marshal(doc, toonenc.WithIndent(2))
	}
	if err != nil {
		t.Fatalf("encoding role-collections file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing role-collections file: %v", err)
	}
	return path
}

func testRCDoc(orgGUID, orgName, region string) rcOutDoc {
	return rcOutDoc{Regions: []rcOutRegion{{ID: region, Orgs: []rcOutOrg{{ID: orgGUID, Name: orgName, RoleCollections: []rcOutRoleCollection{
		{Name: "Collection A", Description: "d1", RoleReferences: []rcOutRoleRef{
			{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "Role A", Description: "rd1"},
		}},
		{Name: "Collection B", Description: "d2"},
	}}}}}}
}

func TestParseRoleCollectionsFile_JSONAndTOON(t *testing.T) {
	doc := testRCDoc("org1", "my-org", "us10")
	for _, format := range []string{"json", "toon"} {
		path := writeRCFile(t, format, doc)
		entries, err := parseRoleCollectionsFile(path)
		if err != nil {
			t.Fatalf("[%s] parseRoleCollectionsFile failed: %v", format, err)
		}
		if len(entries) != 2 {
			t.Fatalf("[%s] expected 2 entries, got %d", format, len(entries))
		}
		if entries[0].RC.Name != "Collection A" || len(entries[0].RC.RoleReferences) != 1 {
			t.Errorf("[%s] unexpected first entry: %+v", format, entries[0])
		}
	}
}

func TestCreateRoleCollections_CreatesAndSkipsExisting(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	calls.conflictNames["Collection B"] = true
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := testRCDoc(orgGUID, orgName, region)
	path := writeRCFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-role-collections", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("create-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "+ [us10] my-org / Collection A") {
		t.Errorf("expected Collection A created, got: %q", stdout)
	}
	if !strings.Contains(stdout, "~ [us10] my-org / Collection B (already exists)") {
		t.Errorf("expected Collection B already-exists, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(calls.created))
	}
	if len(calls.created[0].RoleReferences) != 1 {
		t.Errorf("expected Collection A's role reference to be sent, got: %+v", calls.created[0])
	}
}

func TestUpdateRoleCollections_UpdatesAndWarnsNotFound(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	calls.notFoundNames["Collection B"] = true
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := testRCDoc(orgGUID, orgName, region)
	path := writeRCFile(t, "toon", doc)

	stdout, stderr, err := runCmd(t, "update-role-collections", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("update-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "~ [us10] my-org / Collection A") {
		t.Errorf("expected Collection A updated, got: %q", stdout)
	}
	if !strings.Contains(stderr, "Collection B: not found") {
		t.Errorf("expected Collection B not-found warning, got: %q", stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.updated) != 2 {
		t.Fatalf("expected 2 update calls, got %d", len(calls.updated))
	}
}

func TestDeleteRoleCollections_FromFile(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := testRCDoc(orgGUID, orgName, region)
	path := writeRCFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "delete-role-collections", "*A", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("delete-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "- [us10] my-org / Collection A") {
		t.Errorf("expected Collection A deleted, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedNames) != 1 || calls.deletedNames[0] != "Collection A" {
		t.Errorf("expected only Collection A deleted, got: %v", calls.deletedNames)
	}
}

func TestDeleteRoleCollections_Discovery(t *testing.T) {
	const orgGUID = "org1"
	rcSrv, calls := newRCAdminServer(t, []xsuaa.RoleCollection{
		{Name: "Custom_Viewer"},
		{Name: "GlobalAdmin"},
	})
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rcSrv.URL)
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	stdout, stderr, err := runCmd(t, "delete-role-collections", "Custom_*", "--yes", "--no-prompt")
	if err != nil {
		t.Fatalf("delete-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Custom_Viewer") {
		t.Errorf("expected Custom_Viewer deleted, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedNames) != 1 || calls.deletedNames[0] != "Custom_Viewer" {
		t.Errorf("expected only Custom_Viewer deleted, got: %v", calls.deletedNames)
	}
}

func TestDeleteRoleCollections_RequiresPatternWithoutFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "delete-role-collections")
	if err == nil {
		t.Fatal("expected error when neither a pattern nor --role-collections is given")
	}
}
