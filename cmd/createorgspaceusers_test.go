package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// writeOspUsersCSV writes a CSV in the org-space-users --format csv layout,
// which is the --users input format for create-org-space-users and
// delete-org-space-users.
// Header: region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles
func writeOspUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "osp-users-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles\n") //nolint:errcheck
	for _, row := range rows {
		f.WriteString(strings.Join(row, ",") + "\n") //nolint:errcheck
	}
	return f.Name()
}

// writeBroadcastUsersCSV writes a 3-column broadcast CSV (no region/org/space targeting).
func writeBroadcastUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "broadcast-users-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("cfuser_name,cfuser_origin,cfuser_roles\n") //nolint:errcheck
	for _, row := range rows {
		f.WriteString(strings.Join(row, ",") + "\n") //nolint:errcheck
	}
	return f.Name()
}

func TestCreateOrgSpaceUsers_MissingUsersFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "create-org-space-users")
	if err == nil {
		t.Fatal("expected error when CSV argument is not provided")
	}
}

func TestCreateOrgSpaceUsers_InvalidUsersCSV(t *testing.T) {
	setupTestEnv(t, "http://fake-cf.example.com")

	// Write a file with an unrecognised header (neither 3-column nor 9-column).
	f, err := os.CreateTemp(t.TempDir(), "bad-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("name,origin,roles\nalice@example.com,sap.ids,organization_manager\n") //nolint:errcheck
	f.Close()

	_, _, err = runCmd(t, "create-org-space-users", f.Name(), "--yes")
	if err == nil {
		t.Fatal("expected error for unrecognized CSV header")
	}
	if !strings.Contains(err.Error(), "invalid users CSV") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateOrgSpaceUsers_InvalidOrgsCSV(t *testing.T) {
	setupTestEnv(t, "http://fake-cf.example.com")

	usersFile := writeOspUsersCSV(t,
		[]string{"http://fake-cf.example.com", "org1", "my-org", "", "", "", "alice@example.com", "sap.ids", "organization_manager"},
	)
	nonexistent := t.TempDir() + "/nonexistent.csv"

	_, _, err := runCmd(t, "create-org-space-users", usersFile, "--orgs", nonexistent, "--yes")
	if err == nil {
		t.Fatal("expected error for nonexistent --orgs CSV")
	}
}

func TestCreateOrgSpaceUsers_AutoConfirm_OrgLevel(t *testing.T) {
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Org-level row: space_id is empty → CreateOrganizationRole
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "", "", "", "alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "create-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users --yes failed: %v", err)
	}
	if postCount == 0 {
		t.Error("expected at least one POST to /v3/roles for org-level creation")
	}
}

func TestCreateOrgSpaceUsers_AutoConfirm_SpaceLevel(t *testing.T) {
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Space-level row: space_id is non-empty → CreateSpaceRole
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "sp1", "dev", "", "alice@example.com", "sap.ids", "space_developer"},
	)

	_, _, err := runCmd(t, "create-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users --yes (space-level) failed: %v", err)
	}
	if postCount == 0 {
		t.Error("expected at least one POST to /v3/roles for space-level creation")
	}
}

func TestCreateOrgSpaceUsers_OrgFilter(t *testing.T) {
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v3/roles" && r.Method == "POST" {
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
			return
		}
		http.Error(w, "no route: "+r.URL.Path, 404)
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Two rows: one for org-one (included), one for org-two (excluded by --orgs).
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "org-one", "", "", "", "alice@example.com", "sap.ids", "organization_manager"},
		[]string{srv.URL, "org2", "org-two", "", "", "", "alice@example.com", "sap.ids", "organization_manager"},
	)

	// Orgs filter CSV — only include org-one.
	orgsFile, err := os.CreateTemp(t.TempDir(), "orgs-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	orgsFile.WriteString("region,org_id,org_name\n,,org-one\n") //nolint:errcheck
	orgsFile.Close()

	_, _, err = runCmd(t, "create-org-space-users", usersFile, "--orgs", orgsFile.Name(), "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users --orgs filter failed: %v", err)
	}
	// Only one POST should have been made (for org-one only).
	if postCount != 1 {
		t.Errorf("expected exactly 1 POST for org-one, got %d", postCount)
	}
}

// TestCreateOrgSpaceUsers_Broadcast_OrgLevel tests the 3-column broadcast CSV
// format where region/org_id are empty. The command should discover orgs from
// the CF API and create org-level roles in each.
func TestCreateOrgSpaceUsers_Broadcast_OrgLevel(t *testing.T) {
	orgListCalled := false
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations" && r.Method == "GET":
			orgListCalled = true
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
				"resources":  []map[string]interface{}{{"guid": "org1", "name": "my-org"}},
			})))
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// 3-column broadcast CSV — no region/org/space targeting.
	usersFile := writeBroadcastUsersCSV(t,
		[]string{"alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "create-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users broadcast failed: %v", err)
	}
	if !orgListCalled {
		t.Error("expected GET /v3/organizations to be called for broadcast")
	}
	if postCount == 0 {
		t.Error("expected at least one POST to /v3/roles")
	}
}

// TestCreateOrgSpaceUsers_Broadcast_SpaceLevel tests the 3-column broadcast CSV
// with space roles. The command should discover orgs, then all spaces in each org,
// and create space roles in each.
func TestCreateOrgSpaceUsers_Broadcast_SpaceLevel(t *testing.T) {
	orgListCalled := false
	spaceListCalled := false
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations" && r.Method == "GET":
			orgListCalled = true
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
				"resources":  []map[string]interface{}{{"guid": "org1", "name": "my-org"}},
			})))
		case r.URL.Path == "/v3/spaces" && r.Method == "GET":
			spaceListCalled = true
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
				"resources": []map[string]interface{}{
					{"guid": "sp1", "name": "dev",
						"relationships": map[string]interface{}{"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}}}},
				},
			})))
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// 3-column broadcast CSV with space role only.
	usersFile := writeBroadcastUsersCSV(t,
		[]string{"alice@example.com", "sap.ids", "space_developer"},
	)

	_, _, err := runCmd(t, "create-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users broadcast (space) failed: %v", err)
	}
	if !orgListCalled {
		t.Error("expected GET /v3/organizations to be called for broadcast")
	}
	if !spaceListCalled {
		t.Error("expected GET /v3/spaces to be called for space-role broadcast")
	}
	if postCount == 0 {
		t.Error("expected at least one POST to /v3/roles")
	}
}
