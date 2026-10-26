package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// cskServerCalls records what a newCskServer received, for test assertions.
type cskServerCalls struct {
	mu      sync.Mutex
	created []map[string]interface{} // decoded POST bodies to /v3/service_credential_bindings
	deleted []string                 // binding GUIDs DELETEd
}

func (c *cskServerCalls) addCreated(body map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, body)
}

func (c *cskServerCalls) addDeleted(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, guid)
}

// cskInstance describes one managed service instance for newCskServer.
type cskInstance struct {
	GUID                   string
	Name                   string
	SpaceGUID              string
	PlanGUID               string
	Tags                   []string
	ExistingKeyBindingGUID string // non-empty: a binding with the test's key name already exists
}

// newCskServer serves the CF endpoints create-service-keys needs: one org,
// one space, a configurable set of managed service instances (each with an
// optional pre-existing key binding), a single shared plan/offering, and
// handlers for finding/creating/deleting service credential bindings and
// fetching their credential details.
func newCskServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, instances []cskInstance, keyName string) (*httptest.Server, *cskServerCalls) {
	t.Helper()
	calls := &cskServerCalls{}

	existingByInstance := make(map[string]string, len(instances))
	for _, inst := range instances {
		if inst.ExistingKeyBindingGUID != "" {
			existingByInstance[inst.GUID] = inst.ExistingKeyBindingGUID
		}
	}

	instanceResources := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		instanceResources[i] = map[string]interface{}{
			"guid":           inst.GUID,
			"name":           inst.Name,
			"tags":           inst.Tags,
			"last_operation": map[string]interface{}{"state": "succeeded"},
			"relationships": map[string]interface{}{
				"space":        map[string]interface{}{"data": map[string]string{"guid": inst.SpaceGUID}},
				"service_plan": map[string]interface{}{"data": map[string]string{"guid": inst.PlanGUID}},
			},
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, orgName))) //nolint:errcheck

		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, spaceName, orgGUID))) //nolint:errcheck

		case r.URL.Path == "/v3/service_instances" && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  instanceResources,
			})))

		case r.URL.Path == "/v3/service_plans" && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{
						"guid": "plan1",
						"name": "hdi-shared",
						"relationships": map[string]interface{}{
							"service_offering": map[string]interface{}{"data": map[string]string{"guid": "off1"}},
						},
					},
				},
				"included": map[string]interface{}{
					"service_offerings": []map[string]string{{"guid": "off1", "name": "hana"}},
				},
			})))

		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodGet:
			instGUID := r.URL.Query().Get("service_instance_guids")
			name := r.URL.Query().Get("names")
			if bguid, ok := existingByInstance[instGUID]; ok && name == keyName {
				w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
					"pagination": map[string]interface{}{"total_pages": 1},
					"resources":  []map[string]string{{"guid": bguid, "name": name}},
				})))
				return
			}
			w.Write([]byte(emptyPage())) //nolint:errcheck

		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var decoded map[string]interface{}
			json.Unmarshal(body, &decoded) //nolint:errcheck
			calls.addCreated(decoded)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(mustJSONStr(map[string]interface{}{"guid": "new-binding-guid"}))) //nolint:errcheck

		case strings.HasSuffix(r.URL.Path, "/details") && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"credentials": map[string]interface{}{"user": "u1", "password": "p1", "host": "db.example.com"},
			})))

		case strings.HasPrefix(r.URL.Path, "/v3/service_credential_bindings/") && r.Method == http.MethodDelete:
			guid := strings.TrimPrefix(r.URL.Path, "/v3/service_credential_bindings/")
			calls.addDeleted(guid)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestCreateServiceKeys_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "create-service-keys", "mykey", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestCreateServiceKeys_NoScopeNoFlags(t *testing.T) {
	srv, _ := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}}, "mykey")
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "create-service-keys", "mykey", "--yes")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestCreateServiceKeys_CreateBasic(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", Tags: []string{"hana"}}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	outFile := t.TempDir() + "/out.json"
	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey", "--yes", "-o", outFile)
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected creation summary, got: %q", stdout)
	}

	calls.mu.Lock()
	if len(calls.created) != 1 || calls.created[0]["name"] != "mykey" {
		t.Fatalf("expected 1 create call named 'mykey', got: %+v", calls.created)
	}
	calls.mu.Unlock()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	var records []cskOutputRecord
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("parsing output JSON: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 output record, got %d", len(records))
	}
	r := records[0]
	if r.Org != "my-org" || r.Space != "dev" || r.InstanceName != "my-hdi" || r.InstanceGUID != "inst1" {
		t.Errorf("unexpected record identity fields: %+v", r)
	}
	if r.Label != "hana" || r.Plan != "hdi-shared" {
		t.Errorf("expected label=hana plan=hdi-shared, got label=%q plan=%q", r.Label, r.Plan)
	}
	if r.BindingName != "mykey" || r.BindingGUID != "new-binding-guid" {
		t.Errorf("unexpected binding fields: %+v", r)
	}
	if r.Credentials["user"] != "u1" || r.Credentials["password"] != "p1" {
		t.Errorf("expected credentials to be populated, got: %+v", r.Credentials)
	}
}

func TestCreateServiceKeys_NoMatchingInstances(t *testing.T) {
	srv, _ := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "create-service-keys", "mykey", "--yes", "--service-instances", "*-nonexistent")
	if err == nil {
		t.Fatal("expected error when no service instance matches --service-instances")
	}
}

func TestCreateServiceKeys_AbortsWithoutConfirmation(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	// No --yes, no conflicts, and stdin hits EOF — must abort cleanly.
	stdout, _, err := runCmd(t, "create-service-keys", "mykey")
	if err != nil {
		t.Fatalf("expected a clean abort, not an error: %v", err)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.' in output, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 0 {
		t.Errorf("expected no create calls when aborted, got %d", len(calls.created))
	}
}

func TestCreateServiceKeys_ConflictDefaultsToUpdateWithYes(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", ExistingKeyBindingGUID: "old-binding-1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey", "--yes")
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 0 created, 1 updated, 0 failed.") {
		t.Errorf("expected update summary, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deleted) != 1 || calls.deleted[0] != "old-binding-1" {
		t.Errorf("expected old binding to be deleted, got deleted=%v", calls.deleted)
	}
	if len(calls.created) != 1 {
		t.Errorf("expected the key to be recreated, got %d creates", len(calls.created))
	}
}

func TestCreateServiceKeys_ConflictCreateNewOnlyFlagSkipsExisting(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", ExistingKeyBindingGUID: "old-binding-1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey", "--create-new-keys-only")
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Nothing to do.") {
		t.Errorf("expected 'Nothing to do.' since the only match already has the key, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 0 || len(calls.deleted) != 0 {
		t.Errorf("expected no API calls, got created=%d deleted=%d", len(calls.created), len(calls.deleted))
	}
}

func TestCreateServiceKeys_ConflictPromptUpdate(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", ExistingKeyBindingGUID: "old-binding-1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	defer withStdinLines(t, "u")()

	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey")
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 0 created, 1 updated, 0 failed.") {
		t.Errorf("expected update summary, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deleted) != 1 {
		t.Errorf("expected the existing binding to be deleted, got deleted=%v", calls.deleted)
	}
}

func TestCreateServiceKeys_ConflictPromptCreateOnly(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev", []cskInstance{
		{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", ExistingKeyBindingGUID: "old-binding-1"},
		{GUID: "inst2", Name: "my-hdi-2", SpaceGUID: "sp1", PlanGUID: "plan1"},
	}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	defer withStdinLines(t, "c")()

	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey")
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected only the non-conflicting instance to be created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deleted) != 0 {
		t.Errorf("expected no deletes when choosing create-new-only, got deleted=%v", calls.deleted)
	}
	if len(calls.created) != 1 {
		t.Errorf("expected exactly 1 create, got %d", len(calls.created))
	}
}

func TestCreateServiceKeys_ConflictPromptAbort(t *testing.T) {
	srv, calls := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1", ExistingKeyBindingGUID: "old-binding-1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	defer withStdinLines(t, "a")()

	stdout, stderr, err := runCmd(t, "create-service-keys", "mykey")
	if err != nil {
		t.Fatalf("create-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.', got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 0 || len(calls.deleted) != 0 {
		t.Errorf("expected no API calls after abort, got created=%d deleted=%d", len(calls.created), len(calls.deleted))
	}
}

func TestCreateServiceKeys_SpacesFilterExcludesAll(t *testing.T) {
	srv, _ := newCskServer(t, "org1", "my-org", "sp1", "dev",
		[]cskInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}}, "mykey")
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "create-service-keys", "mykey", "--yes", "--spaces", "nonexistent-space")
	if err == nil {
		t.Fatal("expected error when --spaces matches no space")
	}
}
