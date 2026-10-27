package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dskServerCalls records what a newDskServer received, for test assertions.
type dskServerCalls struct {
	mu              sync.Mutex
	deletedBindings []string
}

func (c *dskServerCalls) addDeletedBinding(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedBindings = append(c.deletedBindings, guid)
}

// dskTestInstance describes one service instance for newDskServer.
type dskTestInstance struct {
	GUID      string
	Name      string
	Type      string // "managed" or "user-provided"
	SpaceGUID string
}

// dskTestBinding describes one service credential binding (key or app) on an
// instance, for newDskServer.
type dskTestBinding struct {
	GUID         string
	Name         string
	Type         string // "key" or "app"
	InstanceGUID string
}

// newDskServer serves the CF endpoints delete-service-keys needs: one org,
// one space, a configurable set of service instances, a configurable set of
// service credential bindings on them, and a handler for deleting bindings.
func newDskServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, instances []dskTestInstance, bindings []dskTestBinding) (*httptest.Server, *dskServerCalls) {
	t.Helper()
	calls := &dskServerCalls{}

	instanceResources := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		instanceResources[i] = map[string]interface{}{
			"guid":           inst.GUID,
			"name":           inst.Name,
			"type":           inst.Type,
			"last_operation": map[string]interface{}{"state": "succeeded"},
			"relationships": map[string]interface{}{
				"space": map[string]interface{}{"data": map[string]string{"guid": inst.SpaceGUID}},
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

		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodGet:
			instGUIDs := strings.Split(r.URL.Query().Get("service_instance_guids"), ",")
			wanted := make(map[string]bool, len(instGUIDs))
			for _, g := range instGUIDs {
				wanted[g] = true
			}
			var resources []map[string]interface{}
			for _, b := range bindings {
				if !wanted[b.InstanceGUID] {
					continue
				}
				resources = append(resources, map[string]interface{}{
					"guid": b.GUID,
					"name": b.Name,
					"type": b.Type,
					"relationships": map[string]interface{}{
						"service_instance": map[string]interface{}{"data": map[string]string{"guid": b.InstanceGUID}},
					},
				})
			}
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  resources,
			})))

		case strings.HasPrefix(r.URL.Path, "/v3/service_credential_bindings/") && r.Method == http.MethodDelete:
			guid := strings.TrimPrefix(r.URL.Path, "/v3/service_credential_bindings/")
			calls.addDeletedBinding(guid)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestDeleteServiceKeys_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "delete-service-keys", "*", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestDeleteServiceKeys_NoScopeNoFlags(t *testing.T) {
	srv, _ := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "delete-service-keys", "*", "--yes")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestDeleteServiceKeys_NoMatchingKeys(t *testing.T) {
	srv, _ := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{{GUID: "key1", Name: "temp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "delete-service-keys", "nonexistent-*", "--yes")
	if err == nil {
		t.Fatal("expected error when no key matches the name patterns")
	}
}

func TestDeleteServiceKeys_AbortsWithoutConfirmation(t *testing.T) {
	srv, calls := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "delete-service-keys", "btp*")
	if err != nil {
		t.Fatalf("expected a clean abort, not an error: %v", err)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.' in output, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedBindings) != 0 {
		t.Errorf("expected no deletes when aborted, got %d", len(calls.deletedBindings))
	}
}

func TestDeleteServiceKeys_MatchesByNamePattern(t *testing.T) {
	srv, calls := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{
			{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"},
			{GUID: "inst2", Name: "my-db", Type: "user-provided", SpaceGUID: "sp1"},
		},
		[]dskTestBinding{
			{GUID: "key1", Name: "btp-key-1", Type: "key", InstanceGUID: "inst1"},
			{GUID: "key2", Name: "temp-key", Type: "key", InstanceGUID: "inst2"},
			{GUID: "app1", Name: "an-app-binding", Type: "app", InstanceGUID: "inst2"},
		})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "delete-service-keys", "btp*,temp*", "--yes")
	if err != nil {
		t.Fatalf("delete-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 2 deleted, 0 failed.") {
		t.Errorf("expected 2 deletions, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	deleted := make(map[string]bool, len(calls.deletedBindings))
	for _, g := range calls.deletedBindings {
		deleted[g] = true
	}
	if !deleted["key1"] || !deleted["key2"] {
		t.Errorf("expected key1 and key2 deleted, got: %v", calls.deletedBindings)
	}
	if deleted["app1"] {
		t.Errorf("did not expect the app binding to be deleted, got: %v", calls.deletedBindings)
	}
}

func TestDeleteServiceKeys_NegatedPattern(t *testing.T) {
	srv, calls := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{
			{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"},
			{GUID: "key2", Name: "other-key", Type: "key", InstanceGUID: "inst1"},
		})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	// '!btp*' selects every key name that does NOT match 'btp*'.
	stdout, stderr, err := runCmd(t, "delete-service-keys", "!btp*", "--yes")
	if err != nil {
		t.Fatalf("delete-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 deleted, 0 failed.") {
		t.Errorf("expected 1 deletion, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedBindings) != 1 || calls.deletedBindings[0] != "key2" {
		t.Errorf("expected only key2 (non-matching 'btp*') deleted, got: %v", calls.deletedBindings)
	}
}

func TestDeleteServiceKeys_SpaceFlagAlias(t *testing.T) {
	srv, calls := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "delete-service-keys", "*", "--yes", "--space", "dev")
	if err != nil {
		t.Fatalf("delete-service-keys failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 deleted, 0 failed.") {
		t.Errorf("expected 1 deletion, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedBindings) != 1 {
		t.Errorf("expected key1 deleted, got: %v", calls.deletedBindings)
	}
}

func TestDeleteServiceKeys_SpacesFilterExcludesAll(t *testing.T) {
	srv, calls := newDskServer(t, "org1", "my-org", "sp1", "dev",
		[]dskTestInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1"}},
		[]dskTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "delete-service-keys", "*", "--yes", "--spaces", "prod")
	if err == nil {
		t.Fatal("expected error when --spaces excludes the only space")
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedBindings) != 0 {
		t.Errorf("expected no deletes, got %d", len(calls.deletedBindings))
	}
}
