package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

func TestSubaccountDests_MissingOrgFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "subaccount-destinations")
	if err == nil {
		t.Fatal("expected error when --org is not provided")
	}
}

func TestSubaccountDests_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "subaccount-destinations", "--org", "my-org")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestSubaccountDests_Default(t *testing.T) {
	const orgGUID = "org1"
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON(),                        // instance destinations
		destinationsJSON("sub-dest-one"),           // subaccount destinations
	)

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, "dev", orgGUID))) //nolint:errcheck
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

	stdout, stderr, err := runCmd(t, "subaccount-destinations", "--org", "my-org", "--no-prompt")
	if err != nil {
		t.Fatalf("subaccount-destinations failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "sub-dest-one") {
		t.Errorf("expected sub-dest-one in output, got: %q", stdout)
	}
}

func TestSubaccountDests_DefaultScope(t *testing.T) {
	const orgGUID = "org1"
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON(),
		destinationsJSON("sub-dest-one"),
	)

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, "dev", orgGUID))) //nolint:errcheck
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
	setDefaultOrgScope(t, cfSrv.URL, orgGUID, "my-org")

	// With no --org/--orgs, the default org scope set via `bo orgs` is used.
	stdout, stderr, err := runCmd(t, "subaccount-destinations", "--no-prompt")
	if err != nil {
		t.Fatalf("subaccount-destinations with default scope failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "sub-dest-one") {
		t.Errorf("expected sub-dest-one in output, got: %q", stdout)
	}
}

func TestSubaccountDests_Filter(t *testing.T) {
	const orgGUID = "org1"
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON(),
		destinationsJSON("alpha-dest", "beta-dest"),
	)

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, "dev", orgGUID))) //nolint:errcheck
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

	stdout, _, err := runCmd(t, "subaccount-destinations", "--org", "my-org", "--filter", "alpha", "--no-prompt")
	if err != nil {
		t.Fatalf("subaccount-destinations --filter failed: %v", err)
	}
	if !strings.Contains(stdout, "alpha-dest") {
		t.Errorf("expected alpha-dest in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "beta-dest") {
		t.Errorf("beta-dest should be filtered out, got: %q", stdout)
	}
}

func TestSubaccountDests_OutputFlag(t *testing.T) {
	const orgGUID = "org1"
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	destSrv := newDestServer(t,
		destinationsJSON(),
		destinationsJSON("sub-dest-one"),
	)

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, "dev", orgGUID))) //nolint:errcheck
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

	outPath := filepath.Join(t.TempDir(), "dests.toon")
	stdout, _, err := runCmd(t, "subaccount-destinations", "--org", "my-org", "--output", outPath, "--no-prompt")
	if err != nil {
		t.Fatalf("subaccount-destinations --output failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no result on stdout when --output is set, got: %q", stdout)
	}
	data, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("reading output file: %v", readErr)
	}
	if !strings.Contains(string(data), "sub-dest-one") {
		t.Errorf("expected sub-dest-one in output file, got: %q", string(data))
	}
}

// TestCreateSubaccountDests_OrgsFlag verifies create-subaccount-destinations
// no longer requires --org: an --orgs CSV of exact org refs resolves the
// target(s) instead.
func TestCreateSubaccountDests_OrgsFlag(t *testing.T) {
	const orgGUID = "org1"
	const spaceGUID = "sp1"
	const instanceGUID = "inst1"

	bulkResp := mustJSONStr([]map[string]interface{}{
		{"name": "new-dest", "status": 201},
	})
	destSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(bulkResp)) //nolint:errcheck
	}))
	defer destSrv.Close()

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, "dev", orgGUID))) //nolint:errcheck
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

	destFile := filepath.Join(t.TempDir(), "dests.json")
	if err := os.WriteFile(destFile, []byte(`[{"Name":"new-dest","Type":"HTTP","URL":"https://example.com"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	orgsFile := filepath.Join(t.TempDir(), "orgs.csv")
	orgsCSV := "region,org_id,org_name\n" + store.APIURLToRegion(cfSrv.URL) + ",org1,my-org\n"
	if err := os.WriteFile(orgsFile, []byte(orgsCSV), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmd(t, "create-subaccount-destinations",
		"--orgs", orgsFile, "--destinations", destFile, "--no-prompt")
	if err != nil {
		t.Fatalf("create-subaccount-destinations --orgs failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "created: new-dest") {
		t.Errorf("expected 'created: new-dest' in output, got: %q", stdout)
	}
}
