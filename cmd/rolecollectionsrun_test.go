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
	mu                 sync.Mutex
	created            []xsuaa.RoleCollection
	descriptionUpdates map[string]string
	addedRefs          map[string][]xsuaa.RoleReference
	removedRefs        map[string][]xsuaa.RoleReference
	deletedNames       []string
	conflictNames      map[string]bool // POST to one of these names returns 409
	notFoundNames      map[string]bool // GET/PUT/DELETE-by-name for one of these names returns 404
	existingRoles      []xsuaa.Role    // GET .../roles returns this; set directly before running a command
}

func (c *rcAdminServerCalls) recordCreate(rc xsuaa.RoleCollection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, rc)
}

func (c *rcAdminServerCalls) recordDescriptionUpdate(name, description string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.descriptionUpdates[name] = description
}

func (c *rcAdminServerCalls) recordAddRef(name string, ref xsuaa.RoleReference) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addedRefs[name] = append(c.addedRefs[name], ref)
}

func (c *rcAdminServerCalls) recordRemoveRef(name string, ref xsuaa.RoleReference) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removedRefs[name] = append(c.removedRefs[name], ref)
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

// newRCAdminServer serves the XSUAA Authorization API endpoints (per
// tmp/api/AuthorizationAPI.yaml) that create-role-collections/
// update-role-collections/delete-role-collections and their discovery mode
// rely on:
//   - GET  /rolecollections              bulk list (existingRCs)
//   - GET  /rolecollections/{name}       single lookup, used by
//     UpdateRoleCollection to fetch current state before diffing
//     roleReferences — entries not in existingRCs default to an empty
//     RoleCollection{Name: name} (200 OK) unless name is in notFoundNames.
//   - POST /rolecollections              create
//   - PUT  /rolecollections/{name}       description-only update
//   - PUT/DELETE /rolecollections/{name}/roles/{appId}/{roleName}/{templateName}
//     add/remove a single role reference
//   - DELETE /rolecollections/{name}     delete the collection
func newRCAdminServer(t *testing.T, existingRCs []xsuaa.RoleCollection) (*httptest.Server, *rcAdminServerCalls) {
	t.Helper()
	calls := &rcAdminServerCalls{
		descriptionUpdates: map[string]string{},
		addedRefs:          map[string][]xsuaa.RoleReference{},
		removedRefs:        map[string][]xsuaa.RoleReference{},
		conflictNames:      map[string]bool{},
		notFoundNames:      map[string]bool{},
	}
	rcsGET := mustJSONStr(existingRCs)
	byName := make(map[string]xsuaa.RoleCollection, len(existingRCs))
	for _, rc := range existingRCs {
		byName[rc.Name] = rc
	}

	const rcBase = "/sap/rest/authorization/v2/rolecollections/"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sap/rest/authorization/v2/roles" && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(calls.existingRoles))) //nolint:errcheck

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

		case strings.HasPrefix(r.URL.Path, rcBase):
			segs := strings.Split(strings.TrimPrefix(r.URL.Path, rcBase), "/")
			name := segs[0]
			switch {
			case len(segs) == 1 && r.Method == http.MethodGet:
				if calls.isNotFound(name) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				rc, ok := byName[name]
				if !ok {
					rc = xsuaa.RoleCollection{Name: name}
				}
				w.Write([]byte(mustJSONStr(rc))) //nolint:errcheck

			case len(segs) == 1 && r.Method == http.MethodPut:
				var body struct {
					Description string `json:"description"`
				}
				json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
				calls.recordDescriptionUpdate(name, body.Description)
				if calls.isNotFound(name) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)

			case len(segs) == 1 && r.Method == http.MethodDelete:
				calls.recordDelete(name)
				if calls.isNotFound(name) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusNoContent)

			case len(segs) == 5 && segs[1] == "roles" && r.Method == http.MethodPut:
				ref := xsuaa.RoleReference{RoleTemplateAppID: segs[2], Name: segs[3], RoleTemplateName: segs[4]}
				calls.recordAddRef(name, ref)
				w.WriteHeader(http.StatusOK)

			case len(segs) == 5 && segs[1] == "roles" && r.Method == http.MethodDelete:
				ref := xsuaa.RoleReference{RoleTemplateAppID: segs[2], Name: segs[3], RoleTemplateName: segs[4]}
				calls.recordRemoveRef(name, ref)
				w.WriteHeader(http.StatusNoContent)

			default:
				http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
			}

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

func TestCreateRoleCollections_OrgScopeMismatch_YesFlagRetargetsAndResolvesAppID(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	// The target subaccount has the referenced role, but under a different
	// appId suffix than the one it was exported with.
	calls.existingRoles = []xsuaa.Role{
		{RoleTemplateAppID: "xsuaa!t999999", RoleTemplateName: "xsuaa_admin", Name: "Role A"},
	}
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	// The file's only org (us20/org2) differs from the current 'bo orgs'
	// scope (us10/org1).
	doc := rcOutDoc{Regions: []rcOutRegion{{ID: "us20", Orgs: []rcOutOrg{{ID: "org2", Name: "other-org", RoleCollections: []rcOutRoleCollection{
		{Name: "Collection A", RoleReferences: []rcOutRoleRef{
			{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "Role A"},
		}},
	}}}}}}
	path := writeRCFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-role-collections", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("create-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "[us10] my-org / Collection A") {
		t.Errorf("expected Collection A retargeted onto the current scope (us10/my-org), got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls.created))
	}
	refs := calls.created[0].RoleReferences
	if len(refs) != 1 || refs[0].RoleTemplateAppID != "xsuaa!t999999" {
		t.Errorf("expected role reference appId resolved to xsuaa!t999999, got: %+v", refs)
	}
}

func TestCreateRoleCollections_PatternFiltersFileEntries(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := testRCDoc(orgGUID, orgName, region) // "Collection A" and "Collection B"
	path := writeRCFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "create-role-collections", "*A", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("create-role-collections failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "+ [us10] my-org / Collection A") {
		t.Errorf("expected Collection A created, got: %q", stdout)
	}
	if strings.Contains(stdout, "Collection B") {
		t.Errorf("expected Collection B to be excluded by the pattern, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0].Name != "Collection A" {
		t.Errorf("expected only Collection A created, got: %+v", calls.created)
	}
}

func TestCreateRoleCollections_PatternMatchesNothing(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, _ := newRCAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := testRCDoc(orgGUID, orgName, region)
	path := writeRCFile(t, "json", doc)

	_, _, err := runCmd(t, "create-role-collections", "nonexistent-*", "--role-collections", path, "--yes")
	if err == nil {
		t.Fatal("expected error when the pattern matches no role collection in the file")
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
	if calls.descriptionUpdates["Collection A"] != "d1" {
		t.Errorf("expected Collection A's description to be updated to %q, got: %+v", "d1", calls.descriptionUpdates)
	}
	if _, updated := calls.descriptionUpdates["Collection B"]; updated {
		t.Errorf("expected Collection B's description update to be skipped (not found), got: %+v", calls.descriptionUpdates)
	}
	if len(calls.addedRefs["Collection A"]) != 1 || calls.addedRefs["Collection A"][0].Name != "Role A" {
		t.Errorf("expected Collection A's role reference to be added, got: %+v", calls.addedRefs)
	}
}

func TestUpdateRoleCollections_RemovesStaleReference(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	// The server's current state for "Collection A" has a role reference
	// ("Stale Role") that is absent from the file being applied — it must be
	// removed, while the file's "Role A" (not yet on the server) must be added.
	rcSrv, calls := newRCAdminServer(t, []xsuaa.RoleCollection{
		{Name: "Collection A", Description: "old", RoleReferences: []xsuaa.RoleReference{
			{RoleTemplateAppID: "xsuaa!t9", RoleTemplateName: "stale_tmpl", Name: "Stale Role"},
		}},
	})
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)

	doc := rcOutDoc{Regions: []rcOutRegion{{ID: region, Orgs: []rcOutOrg{{ID: orgGUID, Name: orgName, RoleCollections: []rcOutRoleCollection{
		{Name: "Collection A", Description: "d1", RoleReferences: []rcOutRoleRef{
			{RoleTemplateAppID: "xsuaa!t1", RoleTemplateName: "xsuaa_admin", Name: "Role A"},
		}},
	}}}}}}
	path := writeRCFile(t, "json", doc)

	_, stderr, err := runCmd(t, "update-role-collections", "--role-collections", path, "--yes")
	if err != nil {
		t.Fatalf("update-role-collections failed: %v\nstderr: %s", err, stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.addedRefs["Collection A"]) != 1 || calls.addedRefs["Collection A"][0].Name != "Role A" {
		t.Errorf("expected Role A to be added, got: %+v", calls.addedRefs)
	}
	if len(calls.removedRefs["Collection A"]) != 1 || calls.removedRefs["Collection A"][0].Name != "Stale Role" {
		t.Errorf("expected Stale Role to be removed, got: %+v", calls.removedRefs)
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

func TestDeleteRoleCollections_DefaultScopeWinsOverOrgsFlag(t *testing.T) {
	const orgGUID, orgName, region = "org1", "my-org", "us10"
	rcSrv, calls := newRCAdminServer(t, nil)
	setupTestEnvWithFullXsuaa(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName, region, rcSrv.URL)
	setDefaultOrgScope(t, "https://api.cf."+region+".hana.ondemand.com", orgGUID, orgName)

	// --orgs is passed too, but the 'bo orgs' default scope always wins now
	// (no more 1/2 choice) — it happens to name the same org here.
	orgsCSVPath := writeCosOrgsCSV(t, [][3]string{{region, orgGUID, orgName}})
	doc := testRCDoc(orgGUID, orgName, region)
	path := writeRCFile(t, "json", doc)

	stdout, stderr, err := runCmd(t, "delete-role-collections", "*A", "--role-collections", path, "--orgs", orgsCSVPath, "--yes")
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
