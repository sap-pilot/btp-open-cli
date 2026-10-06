package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cupsServerCalls records what a newCupsServer received, for test assertions.
type cupsServerCalls struct {
	mu      sync.Mutex
	created []map[string]interface{} // decoded POST bodies
	updated []map[string]interface{} // decoded PATCH bodies, keyed in order received
}

func (c *cupsServerCalls) addCreated(body map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, body)
}

func (c *cupsServerCalls) addUpdated(body map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updated = append(c.updated, body)
}

// newCupsServer serves the CF endpoints create-ups needs: one org, one space
// in it, a canned list of existing user-provided services in that space, and
// handlers for creating/updating user-provided service instances.
func newCupsServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, existing map[string]string) (*httptest.Server, *cupsServerCalls) {
	t.Helper()
	calls := &cupsServerCalls{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, orgName))) //nolint:errcheck

		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON(spaceGUID, spaceName, orgGUID))) //nolint:errcheck

		case r.URL.Path == "/v3/service_instances" && r.Method == http.MethodGet:
			resources := make([]map[string]string, 0, len(existing))
			for name, guid := range existing {
				resources = append(resources, map[string]string{"guid": guid, "name": name})
			}
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  resources,
			})))

		case r.URL.Path == "/v3/service_instances" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var decoded map[string]interface{}
			json.Unmarshal(body, &decoded) //nolint:errcheck
			calls.addCreated(decoded)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(mustJSONStr(map[string]interface{}{"guid": "new-guid", "name": decoded["name"]}))) //nolint:errcheck

		case strings.HasPrefix(r.URL.Path, "/v3/service_instances/") && r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var decoded map[string]interface{}
			json.Unmarshal(body, &decoded) //nolint:errcheck
			calls.addUpdated(decoded)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mustJSONStr(map[string]interface{}{"guid": strings.TrimPrefix(r.URL.Path, "/v3/service_instances/")}))) //nolint:errcheck

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// writeCupsInputFile writes a service-credentials.json-shaped file for tests.
func writeCupsInputFile(t *testing.T, entries ...map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service-credentials.json")
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hanaCupsEntry(name string) map[string]interface{} {
	return map[string]interface{}{
		"org": "src-org", "space": "src-space",
		"label": "hana", "name": name, "plan": "hdi-shared",
		"tags":        []string{"hana", "database"},
		"credentials": map[string]interface{}{"host": "example.com"},
	}
}

func TestCreateUps_RequiresSpace(t *testing.T) {
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))
	_, _, err := runCmd(t, "create-ups", path, "--yes")
	if err == nil {
		t.Fatal("expected error when --space is not provided")
	}
}

func TestCreateUps_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))
	_, _, err := runCmd(t, "create-ups", path, "--space", "dev", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestCreateUps_NoScopeNoFlags(t *testing.T) {
	srv, _ := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	_, _, err := runCmd(t, "create-ups", path, "--space", "dev", "--yes")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestCreateUps_CreateBasic(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "--yes")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected creation summary, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls.created))
	}
	if calls.created[0]["name"] != "my-hdi" {
		t.Errorf("expected service name 'my-hdi', got: %v", calls.created[0]["name"])
	}
	if len(calls.updated) != 0 {
		t.Errorf("expected no update calls, got %d", len(calls.updated))
	}
}

func TestCreateUps_Postfix(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	_, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "--postfix", "-ups", "--yes")
	if err != nil {
		t.Fatalf("create-ups --postfix failed: %v\nstderr: %s", err, stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0]["name"] != "my-hdi-ups" {
		t.Fatalf("expected service named 'my-hdi-ups', got: %+v", calls.created)
	}
}

func TestCreateUps_UpdateExisting(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev",
		map[string]string{"my-hdi": "existing-guid-1"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "--yes")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 0 created, 1 updated, 0 failed.") {
		t.Errorf("expected update summary, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.updated) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls.updated))
	}
	if len(calls.created) != 0 {
		t.Errorf("expected no create calls, got %d", len(calls.created))
	}
}

func TestCreateUps_IncludeExclude(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"), map[string]interface{}{
		"org": "src-org", "space": "src-space",
		"label": "xsuaa", "name": "my-uaa",
		"credentials": map[string]interface{}{"clientid": "x"},
	})

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "--include", "hana", "--yes")
	if err != nil {
		t.Fatalf("create-ups --include failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected only the hana service to be created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0]["name"] != "my-hdi" {
		t.Fatalf("expected only 'my-hdi' created, got: %+v", calls.created)
	}
}

func TestCreateUps_NoMatchingSpace(t *testing.T) {
	srv, _ := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	_, _, err := runCmd(t, "create-ups", path, "--space", "nonexistent-space", "--yes")
	if err == nil {
		t.Fatal("expected error when no org has a space with the given name")
	}
}

func TestCreateUps_AbortsWithoutConfirmation(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	// No --yes, and the test process's stdin is not an interactive answer to
	// the confirmation prompt — readLine hits EOF and the command must abort
	// cleanly rather than create anything.
	stdout, _, err := runCmd(t, "create-ups", path, "--space", "dev")
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
