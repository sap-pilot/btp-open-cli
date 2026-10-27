package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sawServerCalls records what a newSawServer received, for test assertions.
type sawServerCalls struct {
	mu      sync.Mutex
	started []string
	stopped []string
}

func (c *sawServerCalls) addStarted(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, guid)
}

func (c *sawServerCalls) addStopped(guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = append(c.stopped, guid)
}

// sawTestApp describes one app for newSawServer.
type sawTestApp struct {
	GUID      string
	Name      string
	State     string
	SpaceGUID string
}

// newSawServer serves the CF endpoints start-apps/stop-apps need: one org,
// one space, a configurable set of apps, and handlers for the start/stop
// v3 actions.
func newSawServer(t *testing.T, orgGUID, orgName, spaceGUID, spaceName string, apps []sawTestApp) (*httptest.Server, *sawServerCalls) {
	t.Helper()
	calls := &sawServerCalls{}

	appResources := make([]map[string]interface{}, len(apps))
	for i, a := range apps {
		appResources[i] = map[string]interface{}{
			"guid": a.GUID, "name": a.Name, "state": a.State,
			"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-01T00:00:00Z",
			"metadata": map[string]interface{}{"annotations": map[string]string{}},
			"relationships": map[string]interface{}{
				"space": map[string]interface{}{"data": map[string]string{"guid": a.SpaceGUID}},
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

		case r.URL.Path == "/v3/apps" && r.Method == http.MethodGet:
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  appResources,
			})))

		case strings.HasSuffix(r.URL.Path, "/actions/start") && r.Method == http.MethodPost:
			guid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v3/apps/"), "/actions/start")
			calls.addStarted(guid)
			w.Write([]byte(mustJSONStr(map[string]interface{}{"guid": guid, "name": guid, "state": "STARTED"}))) //nolint:errcheck

		case strings.HasSuffix(r.URL.Path, "/actions/stop") && r.Method == http.MethodPost:
			guid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v3/apps/"), "/actions/stop")
			calls.addStopped(guid)
			w.Write([]byte(mustJSONStr(map[string]interface{}{"guid": guid, "name": guid, "state": "STOPPED"}))) //nolint:errcheck

		default:
			http.Error(w, "no route: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestStartApps_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "start-apps", "*", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestStartApps_NoScopeNoFlags(t *testing.T) {
	srv, _ := newSawServer(t, "org1", "my-org", "sp1", "dev",
		[]sawTestApp{{GUID: "app1", Name: "web-1", State: "STOPPED", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "start-apps", "*", "--yes")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestStartApps_NoMatchingApps(t *testing.T) {
	srv, _ := newSawServer(t, "org1", "my-org", "sp1", "dev",
		[]sawTestApp{{GUID: "app1", Name: "web-1", State: "STOPPED", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "start-apps", "nonexistent-*", "--yes")
	if err == nil {
		t.Fatal("expected error when no app matches the name patterns")
	}
}

func TestStartApps_AbortsWithoutConfirmation(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev",
		[]sawTestApp{{GUID: "app1", Name: "web-1", State: "STOPPED", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, _, err := runCmd(t, "start-apps", "web-*")
	if err != nil {
		t.Fatalf("expected a clean abort, not an error: %v", err)
	}
	if !strings.Contains(stdout, "Aborted.") {
		t.Errorf("expected 'Aborted.' in output, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.started) != 0 {
		t.Errorf("expected no starts when aborted, got %d", len(calls.started))
	}
}

func TestStartApps_StartsMatched(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev", []sawTestApp{
		{GUID: "app1", Name: "web-1", State: "STOPPED", SpaceGUID: "sp1"},
		{GUID: "app2", Name: "worker-1", State: "STOPPED", SpaceGUID: "sp1"},
		{GUID: "app3", Name: "keep-this-one", State: "STOPPED", SpaceGUID: "sp1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "start-apps", "web-*,worker-*", "--yes")
	if err != nil {
		t.Fatalf("start-apps failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 2 started, 0 failed.") {
		t.Errorf("expected 2 starts, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	started := make(map[string]bool, len(calls.started))
	for _, g := range calls.started {
		started[g] = true
	}
	if !started["app1"] || !started["app2"] {
		t.Errorf("expected app1 and app2 started, got: %v", calls.started)
	}
	if started["app3"] {
		t.Errorf("did not expect app3 (non-matching name) to be started, got: %v", calls.started)
	}
}

func TestStartApps_NegatedPattern(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev", []sawTestApp{
		{GUID: "app1", Name: "web-1", State: "STOPPED", SpaceGUID: "sp1"},
		{GUID: "app2", Name: "worker-1", State: "STOPPED", SpaceGUID: "sp1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	// '!web-*' selects every app name that does NOT match 'web-*'.
	stdout, stderr, err := runCmd(t, "start-apps", "!web-*", "--yes")
	if err != nil {
		t.Fatalf("start-apps failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 started, 0 failed.") {
		t.Errorf("expected 1 start, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.started) != 1 || calls.started[0] != "app2" {
		t.Errorf("expected only app2 (non-matching 'web-*') started, got: %v", calls.started)
	}
}

func TestStopApps_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "stop-apps", "*", "--yes")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestStopApps_StopsMatched(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev", []sawTestApp{
		{GUID: "app1", Name: "web-1", State: "STARTED", SpaceGUID: "sp1"},
		{GUID: "app2", Name: "keep-running", State: "STARTED", SpaceGUID: "sp1"},
	})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "stop-apps", "web-*", "--yes")
	if err != nil {
		t.Fatalf("stop-apps failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 stopped, 0 failed.") {
		t.Errorf("expected 1 stop, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.stopped) != 1 || calls.stopped[0] != "app1" {
		t.Errorf("expected only app1 stopped, got: %v", calls.stopped)
	}
}

func TestStopApps_SpaceFlagAlias(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev",
		[]sawTestApp{{GUID: "app1", Name: "web-1", State: "STARTED", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	stdout, stderr, err := runCmd(t, "stop-apps", "*", "--yes", "--space", "dev")
	if err != nil {
		t.Fatalf("stop-apps failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Done: 1 stopped, 0 failed.") {
		t.Errorf("expected 1 stop, got: %q", stdout)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.stopped) != 1 {
		t.Errorf("expected app1 stopped, got: %v", calls.stopped)
	}
}

func TestStopApps_SpacesFilterExcludesAll(t *testing.T) {
	srv, calls := newSawServer(t, "org1", "my-org", "sp1", "dev",
		[]sawTestApp{{GUID: "app1", Name: "web-1", State: "STARTED", SpaceGUID: "sp1"}})
	setupTestEnv(t, srv.URL)
	setDefaultOrgScope(t, srv.URL, "org1", "my-org")

	_, _, err := runCmd(t, "stop-apps", "*", "--yes", "--spaces", "prod")
	if err == nil {
		t.Fatal("expected error when --spaces excludes the only space")
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.stopped) != 0 {
		t.Errorf("expected no stops, got %d", len(calls.stopped))
	}
}
