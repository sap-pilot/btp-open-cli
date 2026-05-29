package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// xsuaaRoleCollectionsPage returns a flat JSON array of role collections.
func xsuaaRoleCollectionsPage(names ...string) string {
	rcs := make([]map[string]interface{}, len(names))
	for i, n := range names {
		rcs[i] = map[string]interface{}{
			"name":           n,
			"description":    "desc " + n,
			"isReadOnly":     false,
			"roleReferences": []interface{}{},
		}
	}
	return mustJSONStr(rcs)
}

// newRoleCollectionsServer creates a fake XSUAA server serving role collections.
func newRoleCollectionsServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/rolecollections"):
			w.Write([]byte(xsuaaRoleCollectionsPage(names...))) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/sap/rest/authorization/v2/roles"):
			w.Write([]byte("[]")) //nolint:errcheck
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRoleCollections_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "role-collections", "--no-prompt")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}

func TestRoleCollections_Default(t *testing.T) {
	const orgGUID = "org1"
	rcSrv := newRoleCollectionsServer(t, "GlobalAdmin", "Subaccount_Viewer")
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rcSrv.URL)

	stdout, _, err := runCmd(t, "role-collections", "--no-prompt")
	if err != nil {
		t.Fatalf("role-collections failed: %v", err)
	}
	if !strings.Contains(stdout, "GlobalAdmin") {
		t.Errorf("expected GlobalAdmin in output, got: %q", stdout)
	}
}

func TestRoleCollections_CSV(t *testing.T) {
	const orgGUID = "org1"
	rcSrv := newRoleCollectionsServer(t, "GlobalAdmin")
	cfSrv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage(orgGUID, "my-org"),
	})
	setupTestEnvWithXsuaa(t, cfSrv.URL, orgGUID, rcSrv.URL)

	stdout, _, err := runCmd(t, "role-collections", "--format", "csv", "--no-prompt")
	if err != nil {
		t.Fatalf("role-collections --format csv failed: %v", err)
	}
	if !strings.Contains(stdout, "rolecollection_name") {
		t.Errorf("expected CSV header, got: %q", stdout)
	}
}
