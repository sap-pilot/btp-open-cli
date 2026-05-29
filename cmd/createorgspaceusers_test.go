package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// writeCosUsersCSV writes a CSV with the create-org-space-users format
// (header: cfuser_name,cfuser_origin,cfuser_roles).
func writeCosUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cos-users-*.csv")
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
		t.Fatal("expected error when --users is not provided")
	}
}

func TestCreateOrgSpaceUsers_InvalidOrgsCSV(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")

	usersFile := writeCosUsersCSV(t, []string{"alice@example.com", "sap.ids", "organization_manager"})
	// Point to a file that doesn't exist.
	nonexistent := t.TempDir() + "/nonexistent.csv"

	_, _, err := runCmd(t, "create-org-space-users", "--users", usersFile, "--orgs", nonexistent)
	if err == nil {
		t.Fatal("expected error for nonexistent --orgs CSV")
	}
}

func TestCreateOrgSpaceUsers_AutoConfirm(t *testing.T) {
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(singleOrgPage("org1", "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON("sp1", "dev", "org1"))) //nolint:errcheck
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		case r.URL.Path == "/v3/roles":
			w.Write([]byte(emptyPage())) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	usersFile := writeCosUsersCSV(t,
		[]string{"alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "create-org-space-users", "--users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users --yes failed: %v", err)
	}
	if postCount == 0 {
		t.Error("expected at least one POST to /v3/roles")
	}
}

func TestCreateOrgSpaceUsers_OrgFilter(t *testing.T) {
	// Two orgs in the CF response, only one in the orgs filter CSV.
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]string{
					{"guid": "org1", "name": "org-one"},
					{"guid": "org2", "name": "org-two"},
				},
			})))
		case r.URL.Path == "/v3/spaces":
			w.Write([]byte(spacesPageJSON("sp1", "dev", "org1"))) //nolint:errcheck
		case r.URL.Path == "/v3/roles" && r.Method == "POST":
			postCount++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("{}")) //nolint:errcheck
		case r.URL.Path == "/v3/roles":
			w.Write([]byte(emptyPage())) //nolint:errcheck
		default:
			http.Error(w, "no route: "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Orgs filter CSV — only include org1.
	orgsFile, err := os.CreateTemp(t.TempDir(), "orgs-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	orgsFile.WriteString("region,org_id,org_name\n,,org-one\n") //nolint:errcheck
	orgsFile.Close()

	usersFile := writeCosUsersCSV(t, []string{"alice@example.com", "sap.ids", "organization_manager"})

	_, _, err = runCmd(t, "create-org-space-users", "--users", usersFile, "--orgs", orgsFile.Name(), "--yes")
	if err != nil {
		t.Fatalf("create-org-space-users --orgs filter failed: %v", err)
	}
	if postCount == 0 {
		t.Error("expected at least one POST for the included org")
	}
}
