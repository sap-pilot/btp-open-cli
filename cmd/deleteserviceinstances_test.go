package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dsiServerCalls records what a newDsiServer received, for test assertions.
type dsiServerCalls struct {
	mu               sync.Mutex
	deletedBindings  []string
	deletedInstances []string
}

func (c *dsiServerCalls) addDeletedBinding(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedBindings = append(c.deletedBindings, guid)
}

func (c *dsiServerCalls) addDeletedInstance(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedInstances = append(c.deletedInstances, guid)
}

// dsiInstance describes one service instance (managed or user-provided) for
// newDsiServer, along with any pre-existing binding/key GUIDs on it.
type dsiInstance struct {
	GUID         string
	Name         string
	Type         string // "managed" or "user-provided"
	SpaceGUID    string
	PlanGUID     string // empty for user-provided instances
	BindingGUIDs []string
}

// newDsiServer serves the CF endpoints delete-service-instances needs: one
// org, one space, a configurable set of service instances (each possibly
// already having bindings/keys), a shared plan/offering for managed
// instances, and handlers for listing/deleting bindings and instances.
func newDsiServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, instances []dsiInstance) (*httptest.Server, *dsiServerCalls) {
	t.Helper()
	calls := &dsiServerCalls{}

	bindingOwner := make(map[string]string) // binding guid -> instance guid
	for _, inst := range instances {
		for _, b := range inst.BindingGUIDs {
			bindingOwner[b] = inst.GUID
		}
	}

	instanceResources := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		rel := map[string]interface{}{
			"space": map[string]interface{}{"data": map[string]string{"guid": inst.SpaceGUID}},
		}
		if inst.PlanGUID != "" {
			rel["service_plan"] = map[string]interface{}{"data": map[string]string{"guid": inst.PlanGUID}}
		}
		instanceResources[i] = map[string]interface{}{
			"guid":           inst.GUID,
			"name":           inst.Name,
			"type":           inst.Type,
			"last_operation": map[string]interface{}{"state": "succeeded"},
			"relationships":  rel,
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
			instGUIDs := strings.Split(r.URL.Query().Get("service_instance_guids"), ",")
			wanted := make(map[string]bool, len(instGUIDs))
			for _, g := range instGUIDs {
				wanted[g] = true
			}
			var resources []map[string]interface{}
			for bguid, instGUID := range bindingOwner {
				if !wanted[instGUID] {
					continue
				}
				resources = append(resources, map[string]interface{}{
					"guid": bguid,
					"name": "a-binding",
					"type": "key",
					"relationships": map[string]interface{}{
						"service_instance": map[string]interface{}{"data": map[string]string{"guid": instGUID}},
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

		case strings.HasPrefix(r.URL.Path, "/v3/service_instances/") && r.Method == http.MethodDelete:
			guid := strings.TrimPrefix(r.URL.Path, "/v3/service_instances/")
			calls.addDeletedInstance(guid)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestDeleteServiceInstances_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "delete-service-instances", "*-ups", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestDeleteServiceInstances_NoScopeNoFlags(t *testing.T) {
	srv, _ := newDsiServer(t, "org1", "my-org", "sp1", "dev",
		[]dsiInstance{{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "delete-service-instances", "*-ups", "--yes")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestDeleteServiceInstances_NoMatchingInstances(t *testing.T) {
	srv, _ := newDsiServer(t, "org1", "my-org", "sp1", "dev",
		[]dsiInstance{{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "delete-service-instances", "*-nonexistent", "--yes")
	if err == nil {
		t.Fatal("expected error when no instance matches the name patterns")
	}
}

func TestDeleteServiceInstances_AbortsWithoutConfirmation(t *testing.T) {
	srv, calls := newDsiServer(t, "org1", "my-org", "sp1", "dev",
		[]dsiInstance{{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	// No --yes, and stdin hits EOF — must abort cleanly without deleting.
	stdout, _, err := runCmd(t, "delete-service-instances", "*-ups")
	if err != nil {
		t.Fatalf("expected a clean abort, not an error: %v", err)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.' in output, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedInstances) != 0 {
		t.Errorf("expected no deletes when aborted, got %d", len(calls.deletedInstances))
	}
}

func TestDeleteServiceInstances_DeletesBindingsThenInstance(t *testing.T) {
	srv, calls := newDsiServer(t, "org1", "my-org", "sp1", "dev", []dsiInstance{
		{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1", BindingGUIDs: []string{"b1", "b2"}},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "delete-service-instances", "*-ups", "--yes")
	if err != nil {
		t.Fatalf("delete-service-instances failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 deleted, 0 failed.") {
		t.Errorf("expected deletion summary, got: %q", stdout)
	}
	if !strings.Contains(stdout, "2 binding(s)/key(s) removed") {
		t.Errorf("expected the binding count to be reported, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedBindings) != 2 {
		t.Errorf("expected both bindings to be deleted, got: %v", calls.deletedBindings)
	}
	if len(calls.deletedInstances) != 1 || calls.deletedInstances[0] != "inst1" {
		t.Errorf("expected inst1 to be deleted, got: %v", calls.deletedInstances)
	}
}

func TestDeleteServiceInstances_MatchesManagedAndUserProvided(t *testing.T) {
	srv, calls := newDsiServer(t, "org1", "my-org", "sp1", "dev", []dsiInstance{
		{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1"},
		{GUID: "inst2", Name: "tmp-db", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
		{GUID: "inst3", Name: "keep-this-one", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "delete-service-instances", "*-ups,tmp-*", "--yes")
	if err != nil {
		t.Fatalf("delete-service-instances failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 2 deleted, 0 failed.") {
		t.Errorf("expected 2 deletions, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	deleted := make(map[string]bool, len(calls.deletedInstances))
	for _, g := range calls.deletedInstances {
		deleted[g] = true
	}
	if !deleted["inst1"] || !deleted["inst2"] {
		t.Errorf("expected inst1 and inst2 deleted, got: %v", calls.deletedInstances)
	}
	if deleted["inst3"] {
		t.Errorf("did not expect inst3 (non-matching name) to be deleted, got: %v", calls.deletedInstances)
	}
}

func TestDeleteServiceInstances_SpacesFilterExcludesAll(t *testing.T) {
	srv, calls := newDsiServer(t, "org1", "my-org", "sp1", "dev",
		[]dsiInstance{{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "delete-service-instances", "*-ups", "--yes", "--spaces", "prod")
	if err == nil {
		t.Fatal("expected error when --spaces excludes the only space")
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedInstances) != 0 {
		t.Errorf("expected no deletes, got %d", len(calls.deletedInstances))
	}
}

func TestDeleteServiceInstances_SpaceFlagAlias(t *testing.T) {
	srv, calls := newDsiServer(t, "org1", "my-org", "sp1", "dev",
		[]dsiInstance{{GUID: "inst1", Name: "my-hdi-ups", Type: "user-provided", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "delete-service-instances", "*-ups", "--yes", "--space", "dev")
	if err != nil {
		t.Fatalf("delete-service-instances failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 deleted, 0 failed.") {
		t.Errorf("expected 1 deletion, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.deletedInstances) != 1 {
		t.Errorf("expected inst1 deleted, got: %v", calls.deletedInstances)
	}
}
