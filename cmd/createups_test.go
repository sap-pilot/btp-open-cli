package cmd

import (
	"bufio"
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

// withStdinLines feeds the given lines (answers to [y/N] prompts, in order)
// to readLine for the duration of the test, by swapping the shared
// stdinReader (declared in reauth.go) for one backed by a pipe pre-loaded
// with that input. Call the returned func to restore the original reader —
// typically via defer.
func withStdinLines(t *testing.T, lines ...string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := w.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	orig := stdinReader
	stdinReader = bufio.NewReader(r)
	return func() {
		stdinReader = orig
		r.Close()
	}
}

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

func TestCreateUps_DeclineOverwriteStillCreatesNew(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev",
		map[string]string{"my-hdi": "existing-guid-1"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"), hanaCupsEntry("my-hdi-2"))

	// "y" to the main "Proceed?" prompt, "n" to the separate overwrite
	// prompt — the existing service must be skipped without aborting the
	// still-pending create of the brand-new one.
	defer withStdinLines(t, "y", "n")()

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Skipping existing services") {
		t.Errorf("expected a notice about skipping existing services, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected only the new service to be created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0]["name"] != "my-hdi-2" {
		t.Fatalf("expected only 'my-hdi-2' to be created, got: %+v", calls.created)
	}
	if len(calls.updated) != 0 {
		t.Errorf("expected no update calls after declining the overwrite, got %d", len(calls.updated))
	}
}

func TestCreateUps_DeclineOverwriteWithNoNewServices(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev",
		map[string]string{"my-hdi": "existing-guid-1"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	defer withStdinLines(t, "y", "n")()

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Nothing to do.") {
		t.Errorf("expected 'Nothing to do.', got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 0 || len(calls.updated) != 0 {
		t.Errorf("expected no API calls, got created=%d updated=%d", len(calls.created), len(calls.updated))
	}
}

func TestCreateUps_ExplicitYesAnswersBothPrompts(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev",
		map[string]string{"my-hdi": "existing-guid-1"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"), hanaCupsEntry("my-hdi-2"))

	defer withStdinLines(t, "y", "y")()

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 1 updated, 0 failed.") {
		t.Errorf("expected both the create and the overwrite to proceed, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || len(calls.updated) != 1 {
		t.Errorf("expected 1 create and 1 update, got created=%d updated=%d", len(calls.created), len(calls.updated))
	}
}

// newCupsMultiSpaceServer serves one org with several spaces (name -> guid),
// no pre-existing user-provided services, and records create/update calls
// the same way newCupsServer does.
func newCupsMultiSpaceServer(t *testing.T, orgGUID, orgName string, spaces map[string]string) (*httptest.Server, *cupsServerCalls) {
	t.Helper()
	calls := &cupsServerCalls{}

	spaceResources := make([]map[string]interface{}, 0, len(spaces))
	for name, guid := range spaces {
		spaceResources = append(spaceResources, map[string]interface{}{
			"guid": guid,
			"name": name,
			"relationships": map[string]interface{}{
				"organization": map[string]interface{}{
					"data": map[string]string{"guid": orgGUID},
				},
			},
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage(orgGUID, orgName))) //nolint:errcheck

		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  spaceResources,
			})))

		case r.URL.Path == "/v3/service_instances" && r.Method == http.MethodGet:
			w.Write([]byte(emptyPage())) //nolint:errcheck

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

// cupsCreatedSpaceGUID extracts the target space guid a recorded create call
// was sent with, from its relationships.space.data.guid.
func cupsCreatedSpaceGUID(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	rel, _ := body["relationships"].(map[string]interface{})
	sp, _ := rel["space"].(map[string]interface{})
	data, _ := sp["data"].(map[string]interface{})
	guid, _ := data["guid"].(string)
	if guid == "" {
		t.Fatalf("created call has no relationships.space.data.guid: %v", body)
	}
	return guid
}

func TestCreateUps_NoSpaceFlagUsesEntrySpaceField(t *testing.T) {
	srv, calls := newCupsMultiSpaceServer(t, "org1", "my-org",
		map[string]string{"dev": "sp-dev", "prod": "sp-prod"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	devEntry := hanaCupsEntry("dev-hdi")
	devEntry["space"] = "dev"
	prodEntry := hanaCupsEntry("prod-hdi")
	prodEntry["space"] = "prod"
	path := writeCupsInputFile(t, devEntry, prodEntry)

	stdout, stderr, err := runCmd(t, "create-ups", path, "-y")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 2 created, 0 updated, 0 failed.") {
		t.Errorf("expected both entries created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 2 {
		t.Fatalf("expected 2 created, got %d", len(calls.created))
	}
	byName := make(map[string]map[string]interface{}, 2)
	for _, c := range calls.created {
		byName[c["name"].(string)] = c
	}
	if got := cupsCreatedSpaceGUID(t, byName["dev-hdi"]); got != "sp-dev" {
		t.Errorf("dev-hdi: expected space sp-dev, got %q", got)
	}
	if got := cupsCreatedSpaceGUID(t, byName["prod-hdi"]); got != "sp-prod" {
		t.Errorf("prod-hdi: expected space sp-prod, got %q", got)
	}
}

func TestCreateUps_NoSpaceFlagSkipsEntryWithoutSpaceField(t *testing.T) {
	srv, calls := newCupsMultiSpaceServer(t, "org1", "my-org",
		map[string]string{"dev": "sp-dev"})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	withSpace := hanaCupsEntry("dev-hdi")
	withSpace["space"] = "dev"
	noSpace := hanaCupsEntry("no-space-hdi")
	delete(noSpace, "space")
	path := writeCupsInputFile(t, withSpace, noSpace)

	stdout, stderr, err := runCmd(t, "create-ups", path, "-y")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Errorf("expected only the entry with a space field to be created, got: %q", stdout)
	}
	if !strings.Contains(stderr, "no-space-hdi") {
		t.Errorf("expected a warning naming the skipped entry, got stderr: %q", stderr)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 {
		t.Fatalf("expected 1 created, got %d", len(calls.created))
	}
}

func TestCreateUps_TagsFlagOverridesEntryTags(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")
	path := writeCupsInputFile(t, hanaCupsEntry("my-hdi"))

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "--tags", "hana,database,relational", "-y")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Fatalf("expected 1 created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 {
		t.Fatalf("expected 1 created, got %d", len(calls.created))
	}
	tagsRaw, _ := calls.created[0]["tags"].([]interface{})
	got := make([]string, len(tagsRaw))
	for i, v := range tagsRaw {
		got[i] = v.(string)
	}
	want := []string{"hana", "database", "relational"}
	if len(got) != len(want) {
		t.Fatalf("expected tags %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected tags %v, got %v", want, got)
		}
	}
}

func TestCreateUps_InstanceNameFieldFallback(t *testing.T) {
	srv, calls := newCupsServer(t, "org1", "my-org", "sp1", "dev", nil)
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	// Shaped like create-service-keys' output: no "name" field, but
	// "instance_name" instead.
	entry := map[string]interface{}{
		"org": "src-org", "space": "dev",
		"label": "hana", "instance_name": "my-hdi", "plan": "hdi-shared",
		"tags":        []string{"hana"},
		"credentials": map[string]interface{}{"host": "example.com"},
	}
	path := writeCupsInputFile(t, entry)

	stdout, stderr, err := runCmd(t, "create-ups", path, "--space", "dev", "-y")
	if err != nil {
		t.Fatalf("create-ups failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 created, 0 updated, 0 failed.") {
		t.Fatalf("expected 1 created, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.created) != 1 || calls.created[0]["name"] != "my-hdi" {
		t.Fatalf("expected 1 created named my-hdi, got: %v", calls.created)
	}
}
