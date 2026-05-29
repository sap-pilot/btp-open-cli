package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// writeDeleteOrgUsersCSV writes a CSV with the delete-org-space-users format
// (header: cfuser_name,cfuser_origin).
func writeDeleteOrgUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "del-org-users-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("cfuser_name,cfuser_origin\n") //nolint:errcheck
	for _, row := range rows {
		f.WriteString(strings.Join(row, ",") + "\n") //nolint:errcheck
	}
	return f.Name()
}

func TestDeleteOrgUsers_MissingUsersFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "delete-org-space-users")
	if err == nil {
		t.Fatal("expected error when --users is not provided")
	}
}

func TestDeleteOrgUsers_AutoConfirm(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations":
			// ListOrganizations
			w.Write([]byte(singleOrgPage("org1", "my-org"))) //nolint:errcheck
		case r.URL.Path == "/v3/spaces":
			// ListOrganizationSpaces (query: organization_guids=org1)
			w.Write([]byte(spacesPageJSON("sp1", "dev", "org1"))) //nolint:errcheck
		case r.URL.Path == "/v3/users":
			// FindCfUser (query: usernames=alice@example.com&origins=sap.ids)
			w.Write([]byte(mustJSONStr(map[string]interface{}{
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "u1", "username": "alice@example.com", "origin": "sap.ids"},
				},
			}))) //nolint:errcheck
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			// ListOrganizationUserRoles and ListSpaceUserRoles both hit /v3/roles
			w.Write([]byte(mustJSONStr(map[string]interface{}{
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{
						"guid": "role1",
						"type": "organization_manager",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
							"space":        map[string]interface{}{"data": nil},
						},
					},
				},
			}))) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	usersFile := writeDeleteOrgUsersCSV(t, []string{"alice@example.com", "sap.ids"})

	// Note: the command has a hardcoded 5-second sleep between space and org role
	// deletion phases; the test timeout must be higher (default runCmd uses no timeout).
	_, _, err := runCmd(t, "delete-org-space-users", "--users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users --yes failed: %v", err)
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}
