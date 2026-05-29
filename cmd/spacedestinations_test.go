package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// destinationsJSON returns a JSON array of destination maps.
func destinationsJSON(names ...string) string {
	dests := make([]map[string]string, len(names))
	for i, n := range names {
		dests[i] = map[string]string{
			"Name": n,
			"URL":  "https://example.com/" + n,
			"Type": "HTTP",
		}
	}
	return mustJSONStr(dests)
}

// newDestServer creates a fake destination service API server.
func newDestServer(t *testing.T, instanceDests, subaccountDests string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/destination-configuration/v1/instanceDestinations":
			w.Write([]byte(instanceDests)) //nolint:errcheck
		case "/destination-configuration/v1/subaccountDestinations":
			w.Write([]byte(subaccountDests)) //nolint:errcheck
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSpaceDests_MissingSpaceFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "space-destinations")
	if err == nil {
		t.Fatal("expected error when --space is not provided")
	}
}

func TestSpaceDests_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "space-destinations", "--space", "sp1")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestSpaceDests_Default(t *testing.T) {
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON("my-destination"),
		destinationsJSON(),
	)

	// CF server responds to: FindSpaceByGUID, FindServicePlan, ListServiceInstancesInSpace
	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/spaces" && strings.Contains(r.URL.RawQuery, "guids="+spaceGUID):
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{
						"guid": spaceGUID,
						"name": "dev",
						"relationships": map[string]interface{}{
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
						},
					},
				},
			})))
		case r.URL.Path == "/v3/service_plans":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": "plan1", "name": "lite"}},
			})))
		case r.URL.Path == "/v3/service_instances":
			// The pre-populated cache means this path is still called to enumerate instances.
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": instanceGUID, "name": "dest-instance"}},
			})))
		default:
			http.Error(w, "no route: "+r.URL.Path+"?"+r.URL.RawQuery, 404)
		}
	}))
	defer cfSrv.Close()

	setupTestEnvWithDestCache(t, cfSrv.URL, spaceGUID, instanceGUID, destSrv.URL)

	stdout, stderr, err := runCmd(t, "space-destinations", "--space", spaceGUID)
	if err != nil {
		t.Fatalf("space-destinations failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "my-destination") {
		t.Errorf("expected destination name in output, got: %q", stdout)
	}
}

func TestSpaceDests_Filter(t *testing.T) {
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON("dest-alpha", "dest-beta"),
		destinationsJSON(),
	)

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": spaceGUID, "name": "dev",
						"relationships": map[string]interface{}{
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
						}},
				},
			})))
		case r.URL.Path == "/v3/service_plans":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": "plan1", "name": "lite"}},
			})))
		case r.URL.Path == "/v3/service_instances":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": instanceGUID, "name": "dest-instance"}},
			})))
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer cfSrv.Close()

	setupTestEnvWithDestCache(t, cfSrv.URL, spaceGUID, instanceGUID, destSrv.URL)

	stdout, _, err := runCmd(t, "space-destinations", "--space", spaceGUID, "--filter", "alpha")
	if err != nil {
		t.Fatalf("space-destinations --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "dest-alpha") {
		t.Errorf("expected dest-alpha in filtered output, got: %q", stdout)
	}
	if strings.Contains(stdout, "dest-beta") {
		t.Errorf("dest-beta should be filtered out, got: %q", stdout)
	}
}

func TestSpaceDests_CSV(t *testing.T) {
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t, destinationsJSON("my-dest"), destinationsJSON())
	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": spaceGUID, "name": "dev",
						"relationships": map[string]interface{}{
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
						}},
				},
			})))
		case r.URL.Path == "/v3/service_plans":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": "plan1", "name": "lite"}},
			})))
		case r.URL.Path == "/v3/service_instances":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]string{{"guid": instanceGUID, "name": "dest-instance"}},
			})))
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer cfSrv.Close()

	setupTestEnvWithDestCache(t, cfSrv.URL, spaceGUID, instanceGUID, destSrv.URL)

	stdout, _, err := runCmd(t, "space-destinations", "--space", spaceGUID, "--format", "csv")
	if err != nil {
		t.Fatalf("space-destinations --format csv failed: %v", err)
	}
	// CSV output should contain destination name
	if !strings.Contains(stdout, "my-dest") {
		t.Errorf("expected my-dest in CSV output, got: %q", stdout)
	}
}
