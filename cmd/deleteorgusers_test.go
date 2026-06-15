package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteOrgUsers_MissingUsersFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "delete-org-space-users")
	if err == nil {
		t.Fatal("expected error when --users is not provided")
	}
}

func TestDeleteOrgUsers_InvalidUsersCSV(t *testing.T) {
	setupTestEnv(t, "http://fake-cf.example.com")

	// Write a file with the OLD (incompatible) header.
	f := writeOspUsersCSV(t) // zero rows — just the header, which won't match
	_, _, err := runCmd(t, "delete-org-space-users", "--users", f, "--yes")
	if err == nil {
		t.Fatal("expected error for CSV with no data rows")
	}
}

// TestDeleteOrgUsers_AutoConfirm_OrgLevel tests deletion of an org-level row
// (space_id empty). The 5-second sleep is skipped because there are no space
// rows alongside the org row.
func TestDeleteOrgUsers_AutoConfirm_OrgLevel(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/users":
			// FindCfUser
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]interface{}{{"guid": "u1", "username": "alice@example.com", "origin": "sap.ids"}},
			})))
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			// ListOrganizationUserRoles
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "role1", "type": "organization_manager",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
							"space":        map[string]interface{}{"data": nil},
						}},
				},
			})))
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Org-level row: space_id empty. No 5-second sleep (no space rows).
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "", "", "u1", "alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "delete-org-space-users", "--users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users --yes (org-level) failed: %v", err)
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}

// TestDeleteOrgUsers_AutoConfirm_SpaceLevel tests deletion of a space-level row
// (space_id non-empty). No org rows are present so no 5-second sleep occurs.
func TestDeleteOrgUsers_AutoConfirm_SpaceLevel(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/users":
			// FindCfUser
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]interface{}{{"guid": "u1", "username": "alice@example.com", "origin": "sap.ids"}},
			})))
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			// ListSpaceUserRoles
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "role2", "type": "space_developer",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": nil},
							"space":        map[string]interface{}{"data": map[string]string{"guid": "sp1"}},
						}},
				},
			})))
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Space-level row: space_id = "sp1". No org rows → no 5-second sleep.
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "sp1", "dev", "u1", "alice@example.com", "sap.ids", "space_developer"},
	)

	_, _, err := runCmd(t, "delete-org-space-users", "--users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users --yes (space-level) failed: %v", err)
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}
