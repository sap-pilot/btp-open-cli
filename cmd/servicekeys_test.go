package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// skTestInstance describes one managed service instance for newSkServer.
type skTestInstance struct {
	GUID      string
	Name      string
	SpaceGUID string
	PlanGUID  string
}

// skTestBinding describes one service credential binding (key or app) on an
// instance, for newSkServer.
type skTestBinding struct {
	GUID         string
	Name         string
	Type         string // "key" or "app"
	InstanceGUID string
}

// newSkServer serves the CF endpoints service-keys needs: one org, one
// space, a configurable set of managed service instances, a shared
// plan/offering, a configurable set of service credential bindings, and a
// handler for fetching credential details.
func newSkServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, instances []skTestInstance, bindings []skTestBinding) *httptest.Server {
	t.Helper()

	instanceResources := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		instanceResources[i] = map[string]interface{}{
			"guid":           inst.GUID,
			"name":           inst.Name,
			"type":           "managed",
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
					"guid":       b.GUID,
					"name":       b.Name,
					"type":       b.Type,
					"created_at": "2024-01-01T00:00:00Z",
					"updated_at": "2024-01-02T00:00:00Z",
					"last_operation": map[string]interface{}{
						"type": "create", "state": "succeeded",
					},
					"relationships": map[string]interface{}{
						"service_instance": map[string]interface{}{"data": map[string]string{"guid": b.InstanceGUID}},
					},
				})
			}
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  resources,
			})))

		case strings.HasSuffix(r.URL.Path, "/details") && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"credentials": map[string]interface{}{"user": "u1", "password": "p1", "host": "db.example.com"},
			})))

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceKeys_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "service-keys")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestServiceKeys_NoScopeNoFlags(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev", nil, nil)
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "service-keys")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestServiceKeys_DefaultToonExcludesCredentials(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}},
		[]skTestBinding{
			{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"},
			{GUID: "app1", Name: "an-app-binding", Type: "app", InstanceGUID: "inst1"},
		})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys")
	if err != nil {
		t.Fatalf("service-keys failed: %v", err)
	}
	if !strings.Contains(stdout, "btp-key") {
		t.Errorf("expected key name in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "an-app-binding") {
		t.Errorf("did not expect the app binding in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "db.example.com") {
		t.Errorf("did not expect credentials without --full, got: %q", stdout)
	}
}

func TestServiceKeys_FullIncludesCredentials(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}},
		[]skTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "service-keys", "--full")
	if err != nil {
		t.Fatalf("service-keys --full failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "db.example.com") {
		t.Errorf("expected credentials with --full, got: %q", stdout)
	}
}

func TestServiceKeys_ServiceKeysFilter(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}},
		[]skTestBinding{
			{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"},
			{GUID: "key2", Name: "temp-key", Type: "key", InstanceGUID: "inst1"},
		})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys", "--service-keys", "btp*")
	if err != nil {
		t.Fatalf("service-keys --service-keys failed: %v", err)
	}
	if !strings.Contains(stdout, "btp-key") {
		t.Errorf("expected btp-key in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "temp-key") {
		t.Errorf("did not expect temp-key in filtered output, got: %q", stdout)
	}
}

func TestServiceKeys_ServiceInstancesFilter(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{
			{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"},
			{GUID: "inst2", Name: "other-db", SpaceGUID: "sp1", PlanGUID: "plan1"},
		},
		[]skTestBinding{
			{GUID: "key1", Name: "key-on-hdi", Type: "key", InstanceGUID: "inst1"},
			{GUID: "key2", Name: "key-on-other", Type: "key", InstanceGUID: "inst2"},
		})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys", "--service-instances", "*-hdi")
	if err != nil {
		t.Fatalf("service-keys --service-instances failed: %v", err)
	}
	if !strings.Contains(stdout, "key-on-hdi") {
		t.Errorf("expected key-on-hdi in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "key-on-other") {
		t.Errorf("did not expect key-on-other in filtered output, got: %q", stdout)
	}
}

func TestServiceKeys_CSVFormat(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}},
		[]skTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys", "--format", "csv")
	if err != nil {
		t.Fatalf("service-keys --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "key_name") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
	if !strings.Contains(stdout, "btp-key") {
		t.Errorf("expected key name in CSV, got: %q", stdout)
	}
}

func TestServiceKeys_JSONFormat(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev",
		[]skTestInstance{{GUID: "inst1", Name: "my-hdi", SpaceGUID: "sp1", PlanGUID: "plan1"}},
		[]skTestBinding{{GUID: "key1", Name: "btp-key", Type: "key", InstanceGUID: "inst1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys", "--format", "json")
	if err != nil {
		t.Fatalf("service-keys --format json failed: %v", err)
	}
	if !strings.Contains(stdout, `"key_name": "btp-key"`) {
		t.Errorf("expected key name in JSON, got: %q", stdout)
	}
}

func TestServiceKeys_NoMatches(t *testing.T) {
	srv := newSkServer(t, "org1", "my-org", "sp1", "dev", nil, nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-keys")
	if err != nil {
		t.Fatalf("service-keys with no matches should not error: %v", err)
	}
	if strings.Contains(stdout, "key_name") {
		t.Errorf("expected empty result, got: %q", stdout)
	}
}
