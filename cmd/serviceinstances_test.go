package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// siTestInstance describes one service instance for newSiServer.
type siTestInstance struct {
	GUID      string
	Name      string
	Type      string // "managed" or "user-provided"
	SpaceGUID string
	PlanGUID  string // empty for user-provided instances
	Tags      []string
}

// newSiServer serves the CF endpoints service-instances needs: one org, one
// space, a configurable set of service instances, a shared plan/offering for
// managed instances, and a handler for fetching a user-provided instance's
// credentials.
func newSiServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, instances []siTestInstance) *httptest.Server {
	t.Helper()

	instanceResources := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		rel := map[string]interface{}{
			"space": map[string]interface{}{"data": map[string]string{"guid": inst.SpaceGUID}},
		}
		if inst.PlanGUID != "" {
			rel["service_plan"] = map[string]interface{}{"data": map[string]string{"guid": inst.PlanGUID}}
		}
		instanceResources[i] = map[string]interface{}{
			"guid":       inst.GUID,
			"name":       inst.Name,
			"type":       inst.Type,
			"tags":       inst.Tags,
			"created_at": "2024-01-01T00:00:00Z",
			"updated_at": "2024-01-02T00:00:00Z",
			"last_operation": map[string]interface{}{
				"type": "create", "state": "succeeded",
			},
			"relationships": rel,
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

		case strings.HasSuffix(r.URL.Path, "/credentials") && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{"user": "u1", "password": "p1", "host": "db.example.com"}))) //nolint:errcheck

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceInstances_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "service-instances")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestServiceInstances_NoScopeNoFlags(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "service-instances")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestServiceInstances_DefaultToonExcludesCredentials(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", []siTestInstance{
		{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
		{GUID: "inst2", Name: "my-ups", Type: "user-provided", SpaceGUID: "sp1", Tags: []string{"hana"}},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-instances")
	if err != nil {
		t.Fatalf("service-instances failed: %v", err)
	}
	if !strings.Contains(stdout, "my-hdi") || !strings.Contains(stdout, "my-ups") {
		t.Errorf("expected both instance names in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "db.example.com") {
		t.Errorf("did not expect credentials without --full, got: %q", stdout)
	}
}

func TestServiceInstances_FullIncludesUserProvidedCredentials(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", []siTestInstance{
		{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
		{GUID: "inst2", Name: "my-ups", Type: "user-provided", SpaceGUID: "sp1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "service-instances", "--full")
	if err != nil {
		t.Fatalf("service-instances --full failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "db.example.com") {
		t.Errorf("expected user-provided credentials with --full, got: %q", stdout)
	}
}

func TestServiceInstances_ServiceInstancesFilter(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", []siTestInstance{
		{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
		{GUID: "inst2", Name: "other-db", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-instances", "--service-instances", "*-hdi")
	if err != nil {
		t.Fatalf("service-instances --service-instances failed: %v", err)
	}
	if !strings.Contains(stdout, "my-hdi") {
		t.Errorf("expected my-hdi in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "other-db") {
		t.Errorf("did not expect other-db in filtered output, got: %q", stdout)
	}
}

func TestServiceInstances_CSVFormat(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", []siTestInstance{
		{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1", Tags: []string{"a", "b"}},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-instances", "--format", "csv")
	if err != nil {
		t.Fatalf("service-instances --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "instance_name") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
	if !strings.Contains(stdout, "my-hdi") || !strings.Contains(stdout, "a;b") {
		t.Errorf("expected instance name and joined tags in CSV, got: %q", stdout)
	}
}

func TestServiceInstances_JSONFormat(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", []siTestInstance{
		{GUID: "inst1", Name: "my-hdi", Type: "managed", SpaceGUID: "sp1", PlanGUID: "plan1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-instances", "--format", "json")
	if err != nil {
		t.Fatalf("service-instances --format json failed: %v", err)
	}
	if !strings.Contains(stdout, `"instance_name": "my-hdi"`) {
		t.Errorf("expected instance name in JSON, got: %q", stdout)
	}
}

func TestServiceInstances_NoMatches(t *testing.T) {
	srv := newSiServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "service-instances")
	if err != nil {
		t.Fatalf("service-instances with no matches should not error: %v", err)
	}
	if strings.Contains(stdout, "instance_name") {
		t.Errorf("expected empty result, got: %q", stdout)
	}
}
